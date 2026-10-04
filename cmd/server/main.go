// server は Cloud Run で動く SheepButler 本体。
//
// エンドポイント（Cloud Run の IAM で保護し、Pub/Sub と Cloud Scheduler のサービスアカウントだけが呼べる）:
//
//	POST /pubsub/gmail       Gmail の新着通知（Pub/Sub push）→ 差分を取り込み、必要度を判定する
//	POST /jobs/renew-watch   Gmail の watch を張り直し、判定し残したメールを判定する（Cloud Scheduler から毎日）
//	GET  /healthz            死活確認
//
// ローカルでの確認用に、1回だけ実行して終了するモードもある:
//
//	go run ./cmd/server -once sync          新着を取り込んで判定
//	go run ./cmd/server -once renew-watch   watch を張り直す
//	go run ./cmd/server -once triage        未判定のメールをすべて判定
//	go run ./cmd/server -once backfill -days 30   過去 30 日の受信トレイを取り込んで判定（重みの調整用）
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
	_ "time/tzdata" // distroless イメージにはタイムゾーンのデータが無いため埋め込む

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/Tatsubooom/sheepbutler/internal/config"
	"github.com/Tatsubooom/sheepbutler/internal/envfile"
	"github.com/Tatsubooom/sheepbutler/internal/googleauth"
	"github.com/Tatsubooom/sheepbutler/internal/ingest"
	"github.com/Tatsubooom/sheepbutler/internal/llm"
	"github.com/Tatsubooom/sheepbutler/internal/logx"
	"github.com/Tatsubooom/sheepbutler/internal/mail"
	"github.com/Tatsubooom/sheepbutler/internal/source"
	"github.com/Tatsubooom/sheepbutler/internal/store"
	"github.com/Tatsubooom/sheepbutler/internal/triage"
)

// triageBatch は1回のリクエストで判定するメールの上限。Pub/Sub の応答期限（60秒）に収めるため。
const triageBatch = 10

// envConfig は環境変数から読む設定。判定の設定は config.yaml（CONFIG_PATH）から読む。
type envConfig struct {
	Port       string
	ProjectID  string
	Topic      string
	Sources    []source.Source
	Retention  time.Duration
	ConfigPath string
}

func loadEnv() (envConfig, error) {
	c := envConfig{
		Port:       getenv("PORT", "8080"),
		ProjectID:  os.Getenv("GOOGLE_CLOUD_PROJECT"),
		Topic:      os.Getenv("GMAIL_PUBSUB_TOPIC"),
		ConfigPath: getenv("CONFIG_PATH", "config.yaml"),
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
	once := flag.String("once", "", "sync / renew-watch / triage / backfill を1回だけ実行して終了する（ローカル確認用）")
	days := flag.Int("days", 30, "backfill で取り込む日数")
	flag.Parse()

	log := logx.New(os.Stdout)
	if err := run(*once, *days, log); err != nil {
		log.Error("失敗", "err", err)
		os.Exit(1)
	}
}

func run(once string, days int, log *slog.Logger) error {
	if err := envfile.Load(".env"); err != nil {
		return err
	}
	env, err := loadEnv()
	if err != nil {
		return err
	}
	cfg, err := loadAppConfig(env.ConfigPath, os.Getenv("CONFIG_PATH") != "")
	if err != nil {
		return err
	}
	jst, err := time.LoadLocation("Asia/Tokyo")
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
	fs, err := store.NewFirestore(ctx, env.ProjectID)
	if err != nil {
		return err
	}
	defer fs.Close()

	s := &server{
		log:    log,
		topic:  env.Topic,
		syncer: &ingest.Syncer{Mailbox: gm, Store: fs, Sources: env.Sources, Retention: env.Retention},
		triager: &triage.Triager{
			Store:     fs,
			Mail:      gm,
			LLM:       &llm.Claude{Client: anthropic.NewClient(), Model: cfg.LLM.Model, Structured: cfg.LLM.StructuredOutput}, // ANTHROPIC_API_KEY を読む
			Config:    cfg,
			Loc:       jst,
			Retention: env.Retention,
			Log:       log,
		},
	}

	switch once {
	case "":
	case "sync":
		if err := s.sync(ctx); err != nil {
			return err
		}
		return s.triageAll(ctx)
	case "renew-watch":
		return s.renewWatch(ctx)
	case "triage":
		return s.triageAll(ctx)
	case "backfill":
		res, err := s.syncer.Backfill(ctx, fmt.Sprintf("in:inbox newer_than:%dd", days), 500)
		if err != nil {
			return err
		}
		log.Info("過去のメールを取り込みました", "found", res.Found, "created", res.Created, "skipped", res.Skipped)
		return s.triageAll(ctx)
	default:
		return fmt.Errorf("-once には sync / renew-watch / triage / backfill を指定してください: %q", once)
	}

	srv := &http.Server{
		Addr:              ":" + env.Port,
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	log.Info("起動しました", "port", env.Port, "model", cfg.LLM.Model)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// loadAppConfig は config.yaml を読む。
// CONFIG_PATH を明示したのにファイルが無い場合はエラーにする（設定が使われないまま黙って動くのを防ぐ）。
// 指定していなければ ./config.yaml を探し、無ければ初期値で動かす。
func loadAppConfig(path string, explicit bool) (config.Config, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if explicit {
			return config.Config{}, fmt.Errorf("CONFIG_PATH に指定された設定ファイルがありません: %s", path)
		}
		return config.Load("")
	}
	return config.Load(path)
}

type server struct {
	log     *slog.Logger
	topic   string
	syncer  *ingest.Syncer
	triager *triage.Triager
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
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := s.sync(ctx); err != nil {
		// 500 を返すと Pub/Sub が時間をおいて再送する。
		http.Error(w, "sync failed", http.StatusInternalServerError)
		return
	}
	// 判定の失敗は取り込みの再送では直らないので 204 を返す。判定し残しは次の通知か毎日のジョブで拾う。
	tctx, tcancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer tcancel()
	s.triageBatch(tctx)
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleRenewWatch(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.renewWatch(ctx); err != nil {
		http.Error(w, "renew failed", http.StatusInternalServerError)
		return
	}
	tctx, tcancel := context.WithTimeout(r.Context(), 4*time.Minute)
	defer tcancel()
	s.triageBatch(tctx)
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

// triageBatch は未判定のメールを最大 triageBatch 件判定する。
func (s *server) triageBatch(ctx context.Context) triage.Summary {
	sum, err := s.triager.RunPending(ctx, triageBatch)
	if err != nil {
		s.log.Error("判定に失敗", "err", err)
		return sum
	}
	if sum.Processed+sum.Failed > 0 {
		s.log.Info("判定しました", "processed", sum.Processed, "failed", sum.Failed,
			"urgent", sum.ByTier["urgent"], "digest", sum.ByTier["digest"], "record", sum.ByTier["record"])
	}
	return sum
}

// triageAll は未判定のメールが無くなるまで判定する（ローカル実行用）。
func (s *server) triageAll(ctx context.Context) error {
	for {
		sum := s.triageBatch(ctx)
		if sum.Processed == 0 {
			if sum.Failed > 0 {
				return fmt.Errorf("%d 件の判定に失敗しました。ログを確認してください", sum.Failed)
			}
			return nil
		}
	}
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
