// guard は予算アラートを受け取り、費用が予算に近づいたら SheepButler を止める Cloud Run サービス。
// 本体（cmd/server）とは別のサービスアカウントで動かし、止めるための権限はこちらにだけ与える。
//
//	POST /pubsub/billing   予算アラート（Pub/Sub push）
//	GET  /healthz          死活確認
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Tatsubooom/sheepbutler/internal/guard"
	"github.com/Tatsubooom/sheepbutler/internal/logx"
	"github.com/Tatsubooom/sheepbutler/internal/notify"
)

func main() {
	log := logx.New(os.Stdout)

	g, err := newGuard()
	if err != nil {
		log.Error("起動に失敗", "err", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /pubsub/billing", func(w http.ResponseWriter, r *http.Request) {
		var env struct {
			Message struct {
				Data string `json:"data"`
			} `json:"message"`
		}
		var n guard.Notification
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&env); err == nil {
			if b, err := base64.StdEncoding.DecodeString(env.Message.Data); err == nil {
				json.Unmarshal(b, &n) // 解釈できない通知は予算額 0 として何もしない
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 50*time.Second)
		defer cancel()
		if _, err := g.Handle(ctx, n); err != nil {
			log.Error("停止処理に失敗", "err", err)
			http.Error(w, "guard failed", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	srv := &http.Server{Addr: ":" + getenv("PORT", "8080"), Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Info("ガードを起動しました", "soft", g.SoftRatio, "hard", g.HardRatio)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Error("停止", "err", err)
		os.Exit(1)
	}
}

func newGuard() (*guard.Guard, error) {
	project := os.Getenv("GOOGLE_CLOUD_PROJECT")
	if project == "" {
		return nil, errors.New("GOOGLE_CLOUD_PROJECT が未設定です")
	}
	soft, err := ratio("SOFT_STOP_RATIO", 0.5)
	if err != nil {
		return nil, err
	}
	hard, err := ratio("HARD_STOP_RATIO", 1.0)
	if err != nil {
		return nil, err
	}
	if soft >= hard {
		return nil, fmt.Errorf("SOFT_STOP_RATIO (%v) は HARD_STOP_RATIO (%v) より小さくしてください", soft, hard)
	}

	stopper, err := guard.NewGCP(context.Background(), project, getenv("GUARD_REGION", "asia-northeast1"),
		list(os.Getenv("GUARD_JOBS")), list(os.Getenv("GUARD_SUBSCRIPTIONS")))
	if err != nil {
		return nil, err
	}

	g := &guard.Guard{Stopper: stopper, SoftRatio: soft, HardRatio: hard, Log: logx.New(os.Stdout)}
	// 停止の連絡は Discord が設定されているときだけ行う。
	if tok, ch := os.Getenv("DISCORD_BOT_TOKEN"), os.Getenv("DISCORD_CHANNEL_ID"); tok != "" && ch != "" {
		g.Notifier = &notify.Discord{BotToken: tok, ChannelID: ch}
	}
	return g, nil
}

func ratio(key string, def float64) (float64, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f <= 0 {
		return 0, fmt.Errorf("%s は正の数で指定してください: %q", key, v)
	}
	return f, nil
}

func list(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' }) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
