// chat は Discord のスラッシュコマンドとボタンを受け取る公開サービス（Cloud Run の sheepbutler-chat）。
//
// Discord から呼ばれるため認証なしで公開するが、すべての要求で Discord の署名（Ed25519）を検証し、
// Bot の持ち主以外の操作は拒否する。メールの取り込みや判定は行わない（本体の sheepbutler は非公開のまま）。
//
//	POST /interactions   Discord の Interactions
//	GET  /healthz        死活確認
package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"
	_ "time/tzdata"

	"github.com/Tatsubooom/sheepbutler/internal/calendar"
	"github.com/Tatsubooom/sheepbutler/internal/chat"
	"github.com/Tatsubooom/sheepbutler/internal/config"
	"github.com/Tatsubooom/sheepbutler/internal/discord"
	"github.com/Tatsubooom/sheepbutler/internal/envfile"
	"github.com/Tatsubooom/sheepbutler/internal/googleauth"
	"github.com/Tatsubooom/sheepbutler/internal/logx"
	"github.com/Tatsubooom/sheepbutler/internal/selection"
	"github.com/Tatsubooom/sheepbutler/internal/store"
)

func main() {
	log := logx.New(os.Stdout)
	if err := run(log); err != nil {
		log.Error("失敗", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	if err := envfile.Load(".env"); err != nil {
		return err
	}
	publicKey, owner := os.Getenv("DISCORD_PUBLIC_KEY"), os.Getenv("DISCORD_OWNER_ID")
	if publicKey == "" || owner == "" {
		return errors.New("DISCORD_PUBLIC_KEY と DISCORD_OWNER_ID が必要です（scripts/deploy-chat.sh が設定する）")
	}
	cfg := config.Default()
	if p := os.Getenv("CONFIG_PATH"); p != "" {
		var err error
		if cfg, err = config.Load(p); err != nil {
			return err
		}
	}
	jst, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		return err
	}

	ctx := context.Background()
	fs, err := store.NewFirestore(ctx, os.Getenv("GOOGLE_CLOUD_PROJECT"))
	if err != nil {
		return err
	}
	defer fs.Close()

	h := &chat.Handler{
		Store:     fs,
		Tracker:   &selection.Tracker{Store: fs, Config: cfg, Log: log},
		Proposals: fs,
		Config:    cfg,
		OwnerID:   owner,
		Loc:       jst,
		Log:       log,
	}
	// /ask は本体（非公開）に Pub/Sub で渡す。未設定なら /ask だけ使えない。
	if topic := os.Getenv("CHAT_JOBS_TOPIC"); topic != "" {
		if h.Publisher, err = chat.NewPubSub(ctx, topic); err != nil {
			return err
		}
	} else {
		log.Warn("CHAT_JOBS_TOPIC が未設定のため、/ask は使えません")
	}
	if id := os.Getenv("GOOGLE_CALENDAR_ID"); id != "" {
		if hc, err := googleauth.HTTPClientFromEnv(ctx); err != nil {
			log.Warn("カレンダーの認証情報が無いため、予定は表示しません", "err", err)
		} else if cal, err := calendar.NewGoogle(ctx, hc, id, jst); err == nil {
			h.Calendar = cal
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("POST /interactions", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		// 署名が正しくない要求は 401 で拒否する（Discord もこれを確認する）。
		if err := discord.Verify(publicKey, r.Header.Get("X-Signature-Ed25519"), r.Header.Get("X-Signature-Timestamp"), body, time.Now()); err != nil {
			http.Error(w, "invalid request signature", http.StatusUnauthorized)
			return
		}
		var in discord.Interaction
		if err := json.Unmarshal(body, &in); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2500*time.Millisecond) // Discord は3秒以内の応答を求める
		defer cancel()
		resp := h.Handle(ctx, in)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	srv := &http.Server{Addr: ":" + port, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Info("チャットを起動しました", "port", port)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
