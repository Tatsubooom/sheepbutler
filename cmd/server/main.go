// server は Cloud Run で動く SheepButler 本体。
//
// エンドポイント（Cloud Run の IAM で保護し、Pub/Sub と Cloud Scheduler のサービスアカウントだけが呼べる）:
//
//	POST /pubsub/gmail       Gmail の新着通知（Pub/Sub push）→ 差分を取り込み、必要度を判定する
//	POST /jobs/renew-watch   Gmail の watch を張り直し、判定し残したメールを判定する（Cloud Scheduler から毎日）
//	POST /jobs/tick          即時通知・締切リマインド・まとめ報告（Cloud Scheduler から15分おき）
//	POST /jobs/chat          /ask の質問に答える（チャットのサービスが Pub/Sub 経由で渡す）
//	GET  /healthz            死活確認
//
// ローカルでの確認用に、1回だけ実行して終了するモードもある:
//
//	go run ./cmd/server -once sync          新着を取り込んで判定
//	go run ./cmd/server -once renew-watch   watch を張り直す
//	go run ./cmd/server -once triage        未判定のメールをすべて判定
//	go run ./cmd/server -once backfill -days 30   過去 30 日の受信トレイを取り込んで判定（重みの調整用）
//	go run ./cmd/server -once tick          通知の処理を1回だけ行う
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
	"sync"
	"syscall"
	"time"
	_ "time/tzdata" // distroless イメージにはタイムゾーンのデータが無いため埋め込む

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/Tatsubooom/sheepbutler/internal/assistant"
	"github.com/Tatsubooom/sheepbutler/internal/calendar"
	"github.com/Tatsubooom/sheepbutler/internal/config"
	"github.com/Tatsubooom/sheepbutler/internal/dispatch"
	"github.com/Tatsubooom/sheepbutler/internal/envfile"
	"github.com/Tatsubooom/sheepbutler/internal/googleauth"
	"github.com/Tatsubooom/sheepbutler/internal/ingest"
	"github.com/Tatsubooom/sheepbutler/internal/llm"
	"github.com/Tatsubooom/sheepbutler/internal/logx"
	"github.com/Tatsubooom/sheepbutler/internal/mail"
	"github.com/Tatsubooom/sheepbutler/internal/notify"
	"github.com/Tatsubooom/sheepbutler/internal/selection"
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
	s.store, s.base = fs, cfg
	// 選考状況: 判定の直後に更新し、まとめ報告に載せる。
	s.tracker = &selection.Tracker{Store: fs, Config: cfg, Log: log}
	s.triager.AfterTriage = s.tracker.Apply
	// チャットで設定した志望度（config.yaml に無い企業）も判定に使う。
	s.triager.Interest = func(ctx context.Context, company string) (string, bool) {
		c, err := fs.GetCompany(ctx, selection.CompanyID(company))
		if err != nil || c == nil || c.Interest == "" {
			return "", false
		}
		return c.Interest, true
	}
	if s.dispatcher, err = newDispatcher(ctx, cfg, env, gm, fs, hc, jst, log); err != nil {
		return err
	}
	if s.dispatcher != nil {
		s.dispatcher.Selection = s.tracker
	}
	// /ask の回答。モデルと設定は質問ごとに最新の設定から決める。
	claude := anthropic.NewClient()
	s.assistant = &assistant.Assistant{
		Store: fs, Loc: jst, Log: log,
		Call: func(ctx context.Context, p anthropic.MessageNewParams) (*anthropic.Message, error) {
			return claude.Messages.New(ctx, p)
		},
	}
	if s.dispatcher != nil {
		s.assistant.Calendar = s.dispatcher.Calendar
	}
	s.refreshConfig(ctx)

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
	case "tick":
		if s.dispatcher == nil {
			return errors.New("通知先が設定されていません")
		}
		return s.dispatcher.Tick(ctx)
	case "backfill":
		res, err := s.syncer.Backfill(ctx, fmt.Sprintf("in:inbox newer_than:%dd", days), 500)
		if err != nil {
			return err
		}
		log.Info("過去のメールを取り込みました", "found", res.Found, "created", res.Created, "skipped", res.Skipped)
		return s.triageAll(ctx)
	default:
		return fmt.Errorf("-once には sync / renew-watch / triage / backfill / tick を指定してください: %q", once)
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

// newDispatcher は通知の仕組みを組み立てる。通知先が未設定なら nil を返し、判定までで止める。
func newDispatcher(ctx context.Context, cfg config.Config, env envConfig, gm *mail.Gmail, fs *store.Firestore,
	hc *http.Client, loc *time.Location, log *slog.Logger) (*dispatch.Dispatcher, error) {
	n, err := notify.FromEnv()
	if err != nil {
		log.Warn("通知先が未設定のため、通知は行いません", "err", err)
		return nil, nil
	}
	d := &dispatch.Dispatcher{Store: fs, Notifier: n, Config: cfg, Loc: loc, Retention: env.Retention, Log: log}
	if id := os.Getenv("GOOGLE_CALENDAR_ID"); id != "" {
		cal, err := calendar.NewGoogle(ctx, hc, id, loc)
		if err != nil {
			return nil, err
		}
		d.Calendar = cal
	}
	if account, err := gm.Email(ctx); err == nil {
		d.Link = func(id string) string { return mail.Link(account, id) }
	} else {
		log.Warn("専用 Gmail のアドレスを取得できないため、メールへのリンクは付けません", "err", err)
	}
	return d, nil
}

type server struct {
	log        *slog.Logger
	topic      string
	store      *store.Firestore
	base       config.Config // config.yaml の設定（チャットでの上書きを重ねる前）
	syncer     *ingest.Syncer
	triager    *triage.Triager
	tracker    *selection.Tracker
	dispatcher *dispatch.Dispatcher
	assistant  *assistant.Assistant

	// work は取り込み・判定・通知を1つずつ実行するためのロック。
	// 設定の差し替えと処理が同時に走らないようにし、同じメールを二重に判定・通知するのも防ぐ。
	work sync.Mutex
}

// locked は s.work を持ち、最新の設定を配ってから f を実行する。
func (s *server) locked(ctx context.Context, f func()) {
	s.work.Lock()
	defer s.work.Unlock()
	s.refreshConfig(ctx)
	f()
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /pubsub/gmail", s.handleGmailPush)
	mux.HandleFunc("POST /jobs/renew-watch", s.handleRenewWatch)
	mux.HandleFunc("POST /jobs/tick", s.handleTick)
	mux.HandleFunc("POST /jobs/chat", s.handleChat)
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
	failed := false
	s.locked(r.Context(), func() {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		if err := s.sync(ctx); err != nil {
			failed = true
			return
		}
		// 判定の失敗は取り込みの再送では直らないので 204 を返す。判定し残しは次の通知か毎日のジョブで拾う。
		tctx, tcancel := context.WithTimeout(r.Context(), 35*time.Second)
		defer tcancel()
		s.triageBatch(tctx)
		// 即時通知は判定の直後に送る。失敗しても次の tick で再送する。
		if s.dispatcher != nil {
			dctx, dcancel := context.WithTimeout(r.Context(), 8*time.Second)
			defer dcancel()
			if _, err := s.dispatcher.SendUrgent(dctx); err != nil {
				s.log.Error("即時通知に失敗（次の tick で再送）", "err", err)
			}
		}
	})
	if failed {
		// 500 を返すと Pub/Sub が時間をおいて再送する。
		http.Error(w, "sync failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleTick(w http.ResponseWriter, r *http.Request) {
	if s.dispatcher == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.locked(r.Context(), func() {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		if err := s.dispatcher.Tick(ctx); err != nil {
			// 失敗した処理は次の tick でやり直すので、Scheduler には成功を返して再試行の連打を防ぐ。
			s.log.Error("通知の処理に失敗", "err", err)
		}
	})
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleRenewWatch(w http.ResponseWriter, r *http.Request) {
	var err error
	s.locked(r.Context(), func() {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		if err = s.renewWatch(ctx); err != nil {
			return
		}
		tctx, tcancel := context.WithTimeout(r.Context(), 4*time.Minute)
		defer tcancel()
		s.triageBatch(tctx)
	})
	if err != nil {
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
