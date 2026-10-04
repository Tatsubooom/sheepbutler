package triage

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Tatsubooom/sheepbutler/internal/config"
	"github.com/Tatsubooom/sheepbutler/internal/llm"
	"github.com/Tatsubooom/sheepbutler/internal/model"
)

// maxAttempts を超えて LLM が失敗したメールは、ルールだけで判定して先に進める（無限に再試行しない）。
const maxAttempts = 3

// Store は判定に必要な保存先の操作。
type Store interface {
	ListPending(ctx context.Context, limit int) ([]*model.Message, error)
	SaveTriage(ctx context.Context, id string, ex *llm.Extraction, res model.TriageResult, queue bool) error
	IncrementAttempts(ctx context.Context, id string) (int, error)
	UsageCost(ctx context.Context, day string) (float64, error)
	AddUsage(ctx context.Context, day string, u llm.Usage) error
	AddAudit(ctx context.Context, a model.Audit) error
}

// BodyFetcher はメール本文を取得する。
type BodyFetcher interface {
	Body(ctx context.Context, id string, maxChars int) (string, error)
}

type Triager struct {
	Store     Store
	Mail      BodyFetcher
	LLM       llm.Extractor
	Config    config.Config
	Loc       *time.Location // 日付（1日の費用上限の区切り）と締切の基準。通常は Asia/Tokyo
	Retention time.Duration  // 監査ログの保存期間
	Log       *slog.Logger
	Now       func() time.Time
}

func (t *Triager) now() time.Time {
	if t.Now != nil {
		return t.Now().In(t.Loc)
	}
	return time.Now().In(t.Loc)
}

// Summary は1回の実行結果。
type Summary struct {
	Processed int
	Failed    int
	ByTier    map[model.Tier]int
}

// RunPending は未判定（status=new）のメールを最大 limit 件判定する。
// 1通の失敗で全体を止めず、失敗したメールは次回に再試行する。
func (t *Triager) RunPending(ctx context.Context, limit int) (Summary, error) {
	sum := Summary{ByTier: map[model.Tier]int{}}
	msgs, err := t.Store.ListPending(ctx, limit)
	if err != nil {
		return sum, fmt.Errorf("未判定メールの取得: %w", err)
	}
	for _, m := range msgs {
		res, err := t.TriageOne(ctx, m)
		if err != nil {
			sum.Failed++
			t.Log.Warn("判定に失敗（次回再試行）", "messageId", m.ID, "err", err)
			continue
		}
		sum.Processed++
		sum.ByTier[res.Tier]++
	}
	return sum, nil
}

// TriageOne は1通を判定して保存する。
func (t *Triager) TriageOne(ctx context.Context, m *model.Message) (model.TriageResult, error) {
	now := t.now()
	mail := Mail{From: m.From, Subject: m.Subject, Body: m.Snippet, Source: m.Source}

	if reason, ok := Excluded(t.Config, mail); ok {
		res := model.TriageResult{Tier: model.TierRecord, Reasons: []string{reason}, Method: model.MethodExcluded, At: now}
		return res, t.Store.SaveTriage(ctx, m.ID, nil, res, true)
	}

	// 1日の費用上限を超えていたら LLM を使わない。
	day := now.Format("2006-01-02")
	spent, err := t.Store.UsageCost(ctx, day)
	if err != nil {
		return model.TriageResult{}, fmt.Errorf("費用の確認: %w", err)
	}
	if spent >= t.Config.LLM.DailyBudgetUSD {
		return t.saveRulesOnly(ctx, m, mail, now, fmt.Sprintf("LLM 未使用（本日の費用上限 $%.2f に到達）", t.Config.LLM.DailyBudgetUSD))
	}

	body, err := t.Mail.Body(ctx, m.ID, t.Config.LLM.MaxInputChars)
	if err != nil {
		return model.TriageResult{}, fmt.Errorf("本文の取得: %w", err)
	}
	mail.Body = body

	ex, usage, llmErr := t.LLM.Extract(ctx, llm.Input{
		From: m.From, Subject: m.Subject, Body: body, Now: now, Profile: t.Config.JobHunt.Profile,
	})
	if usage.InputTokens > 0 {
		if err := t.Store.AddUsage(ctx, day, usage); err != nil {
			t.Log.Error("費用の記録に失敗", "err", err)
		}
	}

	var res model.TriageResult
	if llmErr != nil {
		attempts, err := t.Store.IncrementAttempts(ctx, m.ID)
		if err != nil {
			return model.TriageResult{}, err
		}
		t.audit(ctx, m.ID, body, usage, "", llmErr)
		if attempts < maxAttempts {
			return model.TriageResult{}, fmt.Errorf("LLM（%d回目）: %w", attempts, llmErr)
		}
		return t.saveRulesOnly(ctx, m, mail, now, "LLM 未使用（判定に繰り返し失敗）")
	}

	res = Score(t.Config, mail, &ex, now)
	t.audit(ctx, m.ID, body, usage, res.Tier, nil)
	return res, t.Store.SaveTriage(ctx, m.ID, &ex, res, true)
}

func (t *Triager) saveRulesOnly(ctx context.Context, m *model.Message, mail Mail, now time.Time, why string) (model.TriageResult, error) {
	res := Score(t.Config, mail, nil, now)
	res.Reasons = append(res.Reasons, why)
	return res, t.Store.SaveTriage(ctx, m.ID, nil, res, true)
}

func (t *Triager) audit(ctx context.Context, id, body string, u llm.Usage, tier model.Tier, callErr error) {
	a := model.Audit{
		At: t.now(), MessageID: id, Model: u.Model, InputChars: len([]rune(body)),
		InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CostUSD: u.CostUSD, Tier: tier,
		ExpiresAt: t.now().Add(t.Retention),
	}
	if callErr != nil {
		a.Error = callErr.Error()
	}
	if err := t.Store.AddAudit(ctx, a); err != nil {
		t.Log.Error("監査ログの記録に失敗", "err", err)
	}
}
