package chat

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"google.golang.org/api/pubsub/v1"

	"github.com/Tatsubooom/sheepbutler/internal/config"
	"github.com/Tatsubooom/sheepbutler/internal/discord"
	"github.com/Tatsubooom/sheepbutler/internal/model"
	"github.com/Tatsubooom/sheepbutler/internal/selection"
)

// AskJob は /ask の質問を本体（非公開）に渡すための Pub/Sub メッセージ。
// 本体が回答を作り、Token を使って「考え中…」の応答を書き換える（Token は15分だけ有効）。
type AskJob struct {
	ApplicationID string `json:"applicationId"`
	Token         string `json:"token"`
	Question      string `json:"question"`
}

// Publisher は質問を本体に渡す。
type Publisher interface {
	Publish(ctx context.Context, job AskJob) error
}

// PubSub は Pub/Sub のトピックに発行する（REST API。認証は Cloud Run のサービスアカウント）。
type PubSub struct {
	Topic string // projects/<id>/topics/<name>
	svc   *pubsub.Service
}

func NewPubSub(ctx context.Context, topic string) (*PubSub, error) {
	svc, err := pubsub.NewService(ctx)
	if err != nil {
		return nil, err
	}
	return &PubSub{Topic: topic, svc: svc}, nil
}

func (p *PubSub) Publish(ctx context.Context, job AskJob) error {
	b, err := json.Marshal(job)
	if err != nil {
		return err
	}
	_, err = p.svc.Projects.Topics.Publish(p.Topic, &pubsub.PublishRequest{
		Messages: []*pubsub.PubsubMessage{{Data: base64.StdEncoding.EncodeToString(b)}},
	}).Context(ctx).Do()
	return err
}

// maxQuestion は質問の長さの上限（費用を抑えるため）。
const maxQuestion = 500

func (h *Handler) ask(ctx context.Context, in discord.Interaction) discord.Response {
	q := strings.TrimSpace(in.StringOption("question"))
	if q == "" {
		return discord.Message("質問を入力してください。例: /ask 今どこまで進んでる？", true)
	}
	if h.Publisher == nil {
		return discord.Message("チャットの設定が完了していません。", true)
	}
	if r := []rune(q); len(r) > maxQuestion {
		q = string(r[:maxQuestion])
	}
	if err := h.Publisher.Publish(ctx, AskJob{ApplicationID: in.ApplicationID, Token: in.Token, Question: q}); err != nil {
		h.Log.Error("質問を本体に渡せません", "err", err)
		return discord.Message("処理に失敗しました。時間をおいてもう一度試してください。", true)
	}
	// 「考え中…」を表示し、本体が回答で書き換える。
	return discord.Response{Type: discord.RespDeferred, Data: &discord.ResponseData{Flags: discord.FlagEphemeral}}
}

// ProposalStore は変更案の反映に必要な保存先の操作。
type ProposalStore interface {
	GetProposal(ctx context.Context, id string) (*model.Proposal, error)
	TransitionProposal(ctx context.Context, id string, from, to model.ProposalState) (bool, error)
	SetCompanyInterest(ctx context.Context, id, name, interest string) error
	SettingOverrides(ctx context.Context) (map[string]any, error)
	SetSettingOverride(ctx context.Context, key string, value any) error
}

// proposalButton は「反映する」「やめる」ボタンを処理する。custom_id は "prop:<apply|cancel>:<変更案ID>"。
func (h *Handler) proposalButton(ctx context.Context, op, id string) discord.Response {
	p, err := h.Proposals.GetProposal(ctx, id)
	switch {
	case err != nil:
		h.Log.Error("変更案を読めません", "id", id, "err", err)
		return discord.Message("処理に失敗しました。時間をおいてもう一度試してください。", true)
	case p == nil:
		return discord.Message("この変更案は見つかりません（期限切れで削除された可能性があります）。", true)
	case p.State != model.ProposalPending:
		return discord.Message("この変更案は処理済みです。", true)
	case h.now().After(p.ExpiresAt):
		return discord.Message("この変更案は期限切れです。もう一度 /ask で頼んでください。", true)
	}
	p.ID = id // 履歴の ID などに使う。ボタンの ID を正とする

	if op == "cancel" {
		if ok, err := h.Proposals.TransitionProposal(ctx, id, model.ProposalPending, model.ProposalCancelled); err != nil || !ok {
			return discord.Message("この変更案は処理済みです。", true)
		}
		return discord.Message("やめました: "+p.Summary, true)
	}

	// 先に「反映済み」にしてから反映する（二重押しで二重に反映しないように）。失敗したら戻す。
	ok, err := h.Proposals.TransitionProposal(ctx, id, model.ProposalPending, model.ProposalApplied)
	if err != nil || !ok {
		return discord.Message("この変更案は処理済みです。", true)
	}
	result, err := h.applyProposal(ctx, p)
	if err != nil {
		h.Proposals.TransitionProposal(ctx, id, model.ProposalApplied, model.ProposalPending)
		h.Log.Error("変更案の反映に失敗", "id", id, "err", err)
		return discord.Message("反映できませんでした: "+err.Error(), true)
	}
	return discord.Message("✅ 反映しました: "+result, true)
}

func (h *Handler) applyProposal(ctx context.Context, p *model.Proposal) (string, error) {
	switch p.Kind {
	case model.ProposalStatus:
		c, _, err := h.Tracker.SetManual(ctx, p.Company, p.Stage, p.Round, "prop-"+p.ID)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s は今「%s」です", c.Name, c.Label()), nil

	case model.ProposalInterest:
		name := p.Company
		if co, ok := h.Config.FindCompany(name); ok {
			name = co.Name
		}
		if err := h.Proposals.SetCompanyInterest(ctx, selection.CompanyID(name), name, p.Interest); err != nil {
			return "", err
		}
		return p.Summary + "（次に届くメールの判定から使われます）", nil

	case model.ProposalSetting:
		// 今の上書きに重ねても正しい設定になるかを、反映の直前にもう一度確かめる。
		cur, err := h.Proposals.SettingOverrides(ctx)
		if err != nil {
			return "", err
		}
		merged := map[string]any{p.Key: p.Value}
		for k, v := range cur {
			if k != p.Key {
				merged[k] = v
			}
		}
		if _, err := config.ApplyOverrides(h.Config, merged); err != nil {
			return "", err
		}
		if err := h.Proposals.SetSettingOverride(ctx, p.Key, p.Value); err != nil {
			return "", err
		}
		return p.Summary + "（次の処理から使われます）", nil
	}
	return "", fmt.Errorf("知らない変更案の種類です: %s", p.Kind)
}

// AskTimeout は本体が回答を作るまでの上限（Discord の Token は15分有効）。
const AskTimeout = 90 * time.Second
