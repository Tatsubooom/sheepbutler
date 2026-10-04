// server は Cloud Run で動く SheepButler 本体。
//
// エンドポイント（Cloud Run の IAM で保護し、Pub/Sub と Cloud Scheduler のサービスアカウントだけが呼べる）:
//
//	POST /pubsub/gmail       Gmail の新着通知（Pub/Sub push）→ 差分を取り込む
//	POST /jobs/renew-watch   Gmail の watch を張り直す（Cloud Scheduler から毎日）
//	GET  /healthz            死活確認
//
// ローカルでの確認用に、1回だけ実行して終了するモードもある:
//
//	go run ./cmd/server -once sync
//	go run ./cmd/server -once renew-watch
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/Tatsubooom/sheepbutler/internal/envfile"
	"github.com/Tatsubooom/sheepbutler/internal/googleauth"
	"github.com/Tatsubooom/sheepbutler/internal/ingest"
	"github.com/Tatsubooom/sheepbutler/internal/logx"
	"github.com/Tatsubooom/sheepbutler/internal/mail"
	"github.com/Tatsubooom/sheepbutler/internal/source"
	"github.com/Tatsubooom/sheepbutler/internal/store"
)

type config struct {
	Port      string
	ProjectID string
	Topic     string
	Sources   []source.Source
	Retention time.Duration
}

func loadConfig() (config, error) {
	c := config{
		Port:      getenv("PORT", "8080"),
		ProjectID: os.Getenv("GOOGLE_CLOUD_PROJECT"),
		Topic:     os.Getenv("GMAIL_PUBSUB_TOPIC"),
	}
	if c.ProjectID == "" {
		return c, errors.New("GOOGLE_CLOUD_PROJECT が未設定です")
	}
	days, err := strconv.Atoi(getenv("RETENTION_DAYS", "90"))
	if err != nil || days <= 0 {
		return c, fmt.Errorf("RETENTION_DAYS は正の整数で指定してください: %q", os.Getenv("RETENTION_DAYS"))
	}
	c.Retention = time.Duration(days) * 24 * time.Hour
	if c.Sources, err = source.Parse(os.Getenv("MAIL_SOURCES")); err != nil {
		return c, err
	}
	return c, nil
}

func main() {
	once := flag.String("once", "", "sync または renew-watch を1回だけ実行して終了する（ローカル確認用）")
	flag.Parse()

	log := logx.New(os.Stdout)
	if err := run(*once, log); err != nil {
		log.Error("起動に失敗", "err", err)
		os.Exit(1)
	}
}

func run(once string, log *slog.Logger) error {
	if err := envfile.Load(".env"); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	hc, err := googleauth.HTTPClientFromEnv(context.Background())
	if err != nil {
		return err
	}
	gm, err := mail.NewGmail(ctx, hc)
	if err != nil {
		return err
	}
	fs, err := store.NewFirestore(ctx, cfg.ProjectID)
	if err != nil {
		return err
	}
	defer fs.Close()

	s := &server{
		log:    log,
		topic:  cfg.Topic,
		syncer: &ingest.Syncer{Mailbox: gm, Store: fs, Sources: cfg.Sources, Retention: cfg.Retention},
	}

	switch once {
	case "":
	case "sync":
		return s.sync(ctx)
	case "renew-watch":
		return s.renewWatch(ctx)
	default:
		return fmt.Errorf("-once には sync か renew-watch を指定してください: %q", once)
	}

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	log.Info("起動しました", "port", cfg.Port)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

type server struct {
	log    *slog.Logger
	topic  string
	syncer *ingest.Syncer
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /pubsub/gmail", s.handleGmailPush)
	mux.HandleFunc("POST /jobs/renew-watch", s.handleRenewWatch)
	return mux
}

// pushEnvelope は Pub/Sub push の本文。data には Gmail の {"emailAddress", "historyId"} が入る。
type pushEnvelope struct {
	Message struct {
		Data      string `json:"data"`
		MessageID string `json:"messageId"`
	} `json:"message"`
}

type gmailNotification struct {
	HistoryID uint64 `json:"historyId"`
}

func (s *server) handleGmailPush(w http.ResponseWriter, r *http.Request) {
	var env pushEnvelope
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&env); err != nil {
		// 形式が壊れた通知は再送しても直らないので、受け取ったことにして捨てる。
		s.log.Warn("Pub/Sub の本文を解釈できません", "err", err)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var n gmailNotification
	if b, err := base64.StdEncoding.DecodeString(env.Message.Data); err == nil {
		json.Unmarshal(b, &n)
	}
	s.log.Info("Gmail の新着通知", "pubsubMessageId", env.Message.MessageID, "historyId", n.HistoryID)

	// 通知の historyId は使わず、保存済みの位置から同期する（取りこぼしを防ぐため）。
	ctx, cancel := context.WithTimeout(r.Context(), 50*time.Second)
	defer cancel()
	if err := s.sync(ctx); err != nil {
		// 500 を返すと Pub/Sub が時間をおいて再送する。
		http.Error(w, "sync failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleRenewWatch(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.renewWatch(ctx); err != nil {
		http.Error(w, "renew failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) sync(ctx context.Context) error {
	res, err := s.syncer.Sync(ctx)
	if err != nil {
		s.log.Error("同期に失敗", "err", err, "created", res.Created)
		return err
	}
	s.log.Info("同期しました",
		"initialized", res.Initialized, "resynced", res.Resynced,
		"found", res.Found, "created", res.Created, "skipped", res.Skipped)
	return nil
}

func (s *server) renewWatch(ctx context.Context) error {
	if s.topic == "" {
		return errors.New("GMAIL_PUBSUB_TOPIC が未設定です")
	}
	exp, err := s.syncer.RenewWatch(ctx, s.topic)
	if err != nil {
		s.log.Error("watch の更新に失敗", "err", err)
		return err
	}
	s.log.Info("watch を更新しました", "expiresAt", exp)
	return nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
