package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/Tatsubooom/sheepbutler/internal/assistant"
	"github.com/Tatsubooom/sheepbutler/internal/chat"
	"github.com/Tatsubooom/sheepbutler/internal/config"
	"github.com/Tatsubooom/sheepbutler/internal/discord"
)

// currentConfig は config.yaml の設定に、チャットで変えた設定（Firestore の上書き）を重ねて返す。
// 上書きが読めない・不正なときは config.yaml の設定のまま動かす（止めない）。
func (s *server) currentConfig(ctx context.Context) config.Config {
	ov, err := s.store.SettingOverrides(ctx)
	if err != nil {
		s.log.Warn("設定の上書きを読めません。config.yaml の設定で動かします", "err", err)
		return s.base
	}
	if len(ov) == 0 {
		return s.base
	}
	cfg, err := config.ApplyOverrides(s.base, ov)
	if err != nil {
		s.log.Error("設定の上書きが不正です。config.yaml の設定で動かします", "err", err)
		return s.base
	}
	return cfg
}

// refreshConfig は最新の設定を判定・選考状況・通知の各処理に配る。s.work を持った状態で呼ぶ。
func (s *server) refreshConfig(ctx context.Context) {
	cfg := s.currentConfig(ctx)
	s.triager.Config = cfg
	s.tracker.Config = cfg
	if s.dispatcher != nil {
		s.dispatcher.Config = cfg
	}
}

// handleChat は /ask の質問（チャットのサービスが Pub/Sub に発行したもの）に答え、
// Discord の「考え中…」を回答で書き換える。Pub/Sub が再送を繰り返さないよう、常に 204 を返す。
func (s *server) handleChat(w http.ResponseWriter, r *http.Request) {
	defer w.WriteHeader(http.StatusNoContent)
	var env pushEnvelope
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&env); err != nil {
		s.log.Warn("Pub/Sub の本文を解釈できません", "err", err)
		return
	}
	var job chat.AskJob
	b, err := base64.StdEncoding.DecodeString(env.Message.Data)
	if err == nil {
		err = json.Unmarshal(b, &job)
	}
	if err != nil || job.ApplicationID == "" || job.Token == "" || job.Question == "" {
		s.log.Warn("質問の形式が不正です", "err", err)
		return
	}
	if s.assistant == nil {
		s.log.Error("チャットの回答の準備ができていません")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), chat.AskTimeout)
	defer cancel()
	a := *s.assistant // 設定は質問ごとに最新にする
	a.Config = s.currentConfig(ctx)
	a.Model = a.Config.LLM.Model

	var data discord.ResponseData
	ans, err := a.Ask(ctx, job.Question)
	switch {
	case errors.Is(err, assistant.ErrBudget):
		data = chat.AnswerData("本日の LLM 費用の上限に達したため、答えられません。明日また聞いてください（/status などのコマンドは使えます）。", nil)
	case err != nil:
		s.log.Error("回答を作れません", "err", err)
		data = chat.AnswerData("回答を作れませんでした。時間をおいてもう一度試してください。", nil)
	default:
		s.log.Info("質問に答えました", "costUSD", ans.CostUSD, "proposals", len(ans.Proposals))
		data = chat.AnswerData(ans.Text, ans.Proposals)
	}
	// 回答に時間がかかっても書き換えられるよう、書き換えには別の期限を使う。
	ectx, ecancel := context.WithTimeout(context.WithoutCancel(r.Context()), 15*time.Second)
	defer ecancel()
	if err := discord.EditOriginal(ectx, job.ApplicationID, job.Token, data); err != nil {
		s.log.Error("Discord の応答を書き換えられません", "err", err)
	}
}
