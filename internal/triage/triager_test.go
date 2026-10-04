package triage

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Tatsubooom/sheepbutler/internal/config"
	"github.com/Tatsubooom/sheepbutler/internal/llm"
	"github.com/Tatsubooom/sheepbutler/internal/model"
)

type fakeStore struct {
	pending  []*model.Message
	saved    map[string]model.TriageResult
	extract  map[string]*llm.Extraction
	attempts map[string]int
	cost     map[string]float64
	audits   []model.Audit
}

func newFakeStore(msgs ...*model.Message) *fakeStore {
	return &fakeStore{pending: msgs, saved: map[string]model.TriageResult{}, extract: map[string]*llm.Extraction{},
		attempts: map[string]int{}, cost: map[string]float64{}}
}

func (f *fakeStore) ListPending(context.Context, int) ([]*model.Message, error) {
	return f.pending, nil
}
func (f *fakeStore) SaveTriage(_ context.Context, id string, ex *llm.Extraction, res model.TriageResult, _ bool) error {
	f.saved[id], f.extract[id] = res, ex
	return nil
}
func (f *fakeStore) IncrementAttempts(_ context.Context, id string) (int, error) {
	f.attempts[id]++
	return f.attempts[id], nil
}
func (f *fakeStore) UsageCost(_ context.Context, day string) (float64, error) {
	return f.cost[day], nil
}
func (f *fakeStore) AddUsage(_ context.Context, day string, u llm.Usage) error {
	f.cost[day] += u.CostUSD
	return nil
}
func (f *fakeStore) AddAudit(_ context.Context, a model.Audit) error {
	f.audits = append(f.audits, a)
	return nil
}

type fakeBody struct{ calls int }

func (f *fakeBody) Body(context.Context, string, int) (string, error) {
	f.calls++
	return "本文", nil
}

type fakeLLM struct {
	ex    llm.Extraction
	err   error
	calls int
}

func (f *fakeLLM) Extract(context.Context, llm.Input) (llm.Extraction, llm.Usage, error) {
	f.calls++
	return f.ex, llm.Usage{Model: "claude-haiku-4-5", InputTokens: 1000, OutputTokens: 100, CostUSD: 0.0015}, f.err
}

func newTriager(st *fakeStore, l *fakeLLM, body *fakeBody) *Triager {
	cfg := config.Default()
	cfg.Triage.Exclude.SubjectKeywords = []string{"成績"}
	return &Triager{Store: st, Mail: body, LLM: l, Config: cfg, Loc: jst, Retention: 90 * 24 * time.Hour,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return now }}
}

func TestTriageWithLLM(t *testing.T) {
	st := newFakeStore(&model.Message{ID: "a", Subject: "面接日程"})
	l := &fakeLLM{ex: llm.Extraction{MailKind: llm.KindSelection, Importance: 0.8}}
	sum, err := newTriager(st, l, &fakeBody{}).RunPending(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Processed != 1 || st.saved["a"].Tier != model.TierUrgent || st.saved["a"].Method != model.MethodLLM {
		t.Fatalf("sum=%+v saved=%+v", sum, st.saved["a"])
	}
	if st.extract["a"] == nil || len(st.audits) != 1 || st.cost["2026-10-04"] != 0.0015 {
		t.Fatalf("抽出結果・監査ログ・費用を記録する: audits=%d cost=%v", len(st.audits), st.cost)
	}
}

func TestTriageExcludedSkipsLLM(t *testing.T) {
	st := newFakeStore(&model.Message{ID: "a", Subject: "前期成績のお知らせ"})
	l, body := &fakeLLM{}, &fakeBody{}
	newTriager(st, l, body).RunPending(context.Background(), 10)
	if l.calls != 0 || body.calls != 0 {
		t.Fatal("除外したメールは本文も読まず LLM にも送らない")
	}
	if st.saved["a"].Method != model.MethodExcluded || st.saved["a"].Tier != model.TierRecord {
		t.Fatalf("%+v", st.saved["a"])
	}
}

func TestTriageBudgetExceededUsesRules(t *testing.T) {
	st := newFakeStore(&model.Message{ID: "a", Subject: "【重要】お知らせ"})
	st.cost["2026-10-04"] = 0.5 // 上限ちょうど
	l := &fakeLLM{}
	newTriager(st, l, &fakeBody{}).RunPending(context.Background(), 10)
	got := st.saved["a"]
	if l.calls != 0 || got.Method != model.MethodRules || !strings.Contains(strings.Join(got.Reasons, ","), "費用上限") {
		t.Fatalf("calls=%d res=%+v", l.calls, got)
	}
}

func TestTriageRetriesThenFallsBack(t *testing.T) {
	st := newFakeStore(&model.Message{ID: "a", Subject: "件名"})
	l := &fakeLLM{err: errors.New("形式が不正")}
	tr := newTriager(st, l, &fakeBody{})

	for i := 1; i < maxAttempts; i++ {
		sum, _ := tr.RunPending(context.Background(), 10)
		if sum.Failed != 1 {
			t.Fatalf("%d回目は失敗として残し、次回に再試行する", i)
		}
		if _, ok := st.saved["a"]; ok {
			t.Fatalf("%d回目で保存してはいけない", i)
		}
	}
	sum, _ := tr.RunPending(context.Background(), 10)
	if sum.Processed != 1 || st.saved["a"].Method != model.MethodRules {
		t.Fatalf("%d回失敗したらルールだけで判定する: %+v", maxAttempts, st.saved["a"])
	}
	if len(st.audits) != maxAttempts || st.audits[0].Error == "" {
		t.Fatalf("失敗も監査ログに残す: %+v", st.audits)
	}
}
