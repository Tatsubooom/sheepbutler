package chat

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Tatsubooom/sheepbutler/internal/discord"
	"github.com/Tatsubooom/sheepbutler/internal/llm"
	"github.com/Tatsubooom/sheepbutler/internal/model"
	"github.com/Tatsubooom/sheepbutler/internal/selection"
)

var (
	jst = time.FixedZone("JST", 9*3600)
	now = time.Date(2026, 10, 5, 12, 0, 0, 0, jst)
)

type fakeStore struct {
	msgs   map[string]*model.Message
	paused time.Time
}

func (f *fakeStore) GetMessage(_ context.Context, id string) (*model.Message, error) {
	return f.msgs[id], nil
}
func (f *fakeStore) ListByNotify(_ context.Context, st model.NotifyState, _ int) ([]*model.Message, error) {
	var out []*model.Message
	for _, m := range f.msgs {
		if m.Notify == st {
			out = append(out, m)
		}
	}
	return out, nil
}
func (f *fakeStore) ListReceivedSince(context.Context, time.Time) ([]*model.Message, error) {
	var out []*model.Message
	for _, m := range f.msgs {
		out = append(out, m)
	}
	return out, nil
}
func (f *fakeStore) PausedUntil(context.Context) (time.Time, error)      { return f.paused, nil }
func (f *fakeStore) SetPausedUntil(_ context.Context, t time.Time) error { f.paused = t; return nil }

type fakeTracker struct {
	o         selection.Overview
	confirmed []string
	manual    []string
	undoErr   error
}

func (f *fakeTracker) Overview(context.Context, time.Time) (selection.Overview, error) {
	return f.o, nil
}
func (f *fakeTracker) Confirm(_ context.Context, cid, hid string) (*model.Company, bool, error) {
	f.confirmed = append(f.confirmed, cid+"/"+hid)
	return &model.Company{Name: "A社", Stage: model.StageOffer}, true, nil
}
func (f *fakeTracker) Undo(context.Context, string, string) (*model.Company, bool, error) {
	return &model.Company{Name: "A社", Stage: model.StageInterview, Round: 1}, f.undoErr == nil, f.undoErr
}
func (f *fakeTracker) SetManual(_ context.Context, name string, stage model.Stage, round int, hid string) (*model.Company, bool, error) {
	f.manual = append(f.manual, name+"/"+string(stage)+"/"+hid)
	return &model.Company{Name: name, Stage: stage, Round: round}, true, nil
}

func newHandler(st *fakeStore, tr *fakeTracker) *Handler {
	return &Handler{Store: st, Tracker: tr, OwnerID: "owner", Loc: jst,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return now }}
}

func command(name string, opts ...discord.Option) discord.Interaction {
	var in discord.Interaction
	in.Type = discord.TypeApplicationCmd
	in.User = &discord.User{ID: "owner"}
	in.Data.Name = name
	in.Data.Options = opts
	return in
}

func TestOnlyOwner(t *testing.T) {
	in := command("status")
	in.User = &discord.User{ID: "someone-else"}
	r := newHandler(&fakeStore{}, &fakeTracker{}).Handle(context.Background(), in)
	if !strings.Contains(r.Data.Content, "持ち主だけ") {
		t.Fatalf("持ち主以外は拒否する: %q", r.Data.Content)
	}
}

func TestPing(t *testing.T) {
	var in discord.Interaction
	in.Type = discord.TypePing
	if r := newHandler(&fakeStore{}, &fakeTracker{}).Handle(context.Background(), in); r.Type != discord.RespPong {
		t.Fatal("PING には PONG を返す（Discord の URL 確認）")
	}
}

func TestStatus(t *testing.T) {
	tr := &fakeTracker{o: selection.Overview{Active: []*model.Company{
		{Name: "A社", Stage: model.StageInterview, Round: 2, NextAction: model.NextAction{Label: "面接日程の回答", Due: now.Add(48 * time.Hour)}},
	}}}
	r := newHandler(&fakeStore{}, tr).Handle(context.Background(), command("status"))
	if !strings.Contains(r.Data.Content, "• A社 面接（2次）｜次: 面接日程の回答（10/07(水) 12:00まで）") || r.Data.Flags != discord.FlagEphemeral {
		t.Fatalf("%q", r.Data.Content)
	}
}

func TestDeadlines(t *testing.T) {
	m := &model.Message{ID: "a", Subject: "ES提出", Triage: &model.TriageResult{Tier: model.TierDigest},
		Extracted: &llm.Extraction{Summary: "ES提出のお願い", Deadline: now.Add(30 * time.Hour).Format(time.RFC3339)}}
	far := &model.Message{ID: "b", Triage: &model.TriageResult{Tier: model.TierDigest},
		Extracted: &llm.Extraction{Deadline: now.AddDate(0, 0, 20).Format(time.RFC3339)}}
	st := &fakeStore{msgs: map[string]*model.Message{"a": m, "b": far}}
	r := newHandler(st, &fakeTracker{}).Handle(context.Background(), command("deadlines"))
	if !strings.Contains(r.Data.Content, "ES提出のお願い") || strings.Count(r.Data.Content, "•") != 1 {
		t.Fatalf("7日以内だけ: %q", r.Data.Content)
	}
}

func TestPauseResume(t *testing.T) {
	st := &fakeStore{}
	h := newHandler(st, &fakeTracker{})
	h.Handle(context.Background(), command("pause", discord.Option{Name: "hours", Value: 3.0}))
	if !st.paused.Equal(now.Add(3 * time.Hour)) {
		t.Fatalf("3時間止める: %v", st.paused)
	}
	h.Handle(context.Background(), command("pause", discord.Option{Name: "hours", Value: 1000.0}))
	if !st.paused.Equal(now.Add(maxPause)) {
		t.Fatalf("最長7日: %v", st.paused)
	}
	h.Handle(context.Background(), command("resume"))
	if !st.paused.IsZero() {
		t.Fatal("再開する")
	}
}

func button(customID string) discord.Interaction {
	var in discord.Interaction
	in.Type = discord.TypeMessageComponent
	in.User = &discord.User{ID: "owner"}
	in.Data.CustomID = customID
	in.Message = &struct {
		Content string `json:"content"`
	}{Content: "🔴 即時｜内定のご連絡"}
	return in
}

func TestConfirmButton(t *testing.T) {
	st := &fakeStore{msgs: map[string]*model.Message{"m1": {ID: "m1", SelectionCompanyID: "a社"}}}
	tr := &fakeTracker{}
	r := newHandler(st, tr).Handle(context.Background(), button("sel:confirm:m1"))
	if r.Type != discord.RespUpdateMessage || !strings.Contains(r.Data.Content, "✅ 確定しました（A社 は今「内定」）") {
		t.Fatalf("%d %q", r.Type, r.Data.Content)
	}
	if len(r.Data.Components) != 0 || tr.confirmed[0] != "a社/m1" {
		t.Fatalf("ボタンを消し、メールの企業を確定する: %+v %v", r.Data.Components, tr.confirmed)
	}
}

func TestUndoNotLatest(t *testing.T) {
	st := &fakeStore{msgs: map[string]*model.Message{"m1": {ID: "m1", SelectionCompanyID: "a社"}}}
	r := newHandler(st, &fakeTracker{undoErr: selection.ErrNotLatest}).Handle(context.Background(), button("sel:undo:m1"))
	if r.Type != discord.RespMessage || !strings.Contains(r.Data.Content, "取り消せません") {
		t.Fatalf("%q", r.Data.Content)
	}
}

type fakePublisher struct{ jobs []AskJob }

func (f *fakePublisher) Publish(_ context.Context, j AskJob) error {
	f.jobs = append(f.jobs, j)
	return nil
}

func TestAskDefersAndPublishes(t *testing.T) {
	pub := &fakePublisher{}
	h := newHandler(&fakeStore{}, &fakeTracker{})
	h.Publisher = pub
	in := command("ask", discord.Option{Name: "question", Value: "今どこまで進んでる？"})
	in.ApplicationID, in.Token = "app", "tok"
	r := h.Handle(context.Background(), in)
	if r.Type != discord.RespDeferred || r.Data.Flags != discord.FlagEphemeral {
		t.Fatalf("「考え中…」で応答する: %+v", r)
	}
	if len(pub.jobs) != 1 || pub.jobs[0].Question != "今どこまで進んでる？" || pub.jobs[0].Token != "tok" {
		t.Fatalf("質問を本体に渡す: %+v", pub.jobs)
	}
}

type fakeProposals struct {
	ps        map[string]*model.Proposal
	interests map[string]string
	overrides map[string]any
}

func (f *fakeProposals) GetProposal(_ context.Context, id string) (*model.Proposal, error) {
	if p, ok := f.ps[id]; ok {
		cp := *p
		return &cp, nil
	}
	return nil, nil
}
func (f *fakeProposals) TransitionProposal(_ context.Context, id string, from, to model.ProposalState) (bool, error) {
	if f.ps[id].State != from {
		return false, nil
	}
	f.ps[id].State = to
	return true, nil
}
func (f *fakeProposals) SetCompanyInterest(_ context.Context, id, name, interest string) error {
	f.interests[name] = interest
	return nil
}
func (f *fakeProposals) SettingOverrides(context.Context) (map[string]any, error) {
	return f.overrides, nil
}
func (f *fakeProposals) SetSettingOverride(_ context.Context, k string, v any) error {
	f.overrides[k] = v
	return nil
}

func TestProposalButtons(t *testing.T) {
	exp := now.Add(time.Hour)
	fp := &fakeProposals{ps: map[string]*model.Proposal{
		"s": {Kind: model.ProposalStatus, State: model.ProposalPending, Company: "B社", Stage: model.StageDeclined, ExpiresAt: exp},
		"i": {Kind: model.ProposalInterest, State: model.ProposalPending, Company: "C社", Interest: "none", Summary: "C社 を「興味なし」にする", ExpiresAt: exp},
		"g": {Kind: model.ProposalSetting, State: model.ProposalPending, Key: "triage.thresholds.urgent", Value: 0.2, ExpiresAt: exp}, // まとめ(0.3)より下
		"x": {Kind: model.ProposalStatus, State: model.ProposalPending, Company: "D社", Stage: model.StageDeclined, ExpiresAt: now.Add(-time.Minute)},
	}, interests: map[string]string{}, overrides: map[string]any{}}
	tr := &fakeTracker{}
	h := newHandler(&fakeStore{}, tr)
	h.Proposals = fp

	r := h.Handle(context.Background(), button("prop:apply:s"))
	if !strings.Contains(r.Data.Content, "✅ 反映しました: B社 は今「辞退」") || tr.manual[0] != "B社/declined/prop-s" {
		t.Fatalf("選考状況を反映する: %q %v", r.Data.Content, tr.manual)
	}
	if r := h.Handle(context.Background(), button("prop:apply:s")); !strings.Contains(r.Data.Content, "処理済み") || len(tr.manual) != 1 {
		t.Fatal("二重押しで二重に反映しない")
	}

	h.Handle(context.Background(), button("prop:apply:i"))
	if fp.interests["C社"] != "none" {
		t.Fatalf("志望度を反映する: %v", fp.interests)
	}

	r = h.Handle(context.Background(), button("prop:apply:g"))
	if !strings.Contains(r.Data.Content, "反映できませんでした") || len(fp.overrides) != 0 || fp.ps["g"].State != model.ProposalPending {
		t.Fatalf("検証に通らない設定は反映せず、承認待ちに戻す: %q %v", r.Data.Content, fp.overrides)
	}

	if r := h.Handle(context.Background(), button("prop:apply:x")); !strings.Contains(r.Data.Content, "期限切れ") {
		t.Fatalf("期限切れは反映しない: %q", r.Data.Content)
	}
	if r := h.Handle(context.Background(), button("prop:cancel:i")); !strings.Contains(r.Data.Content, "処理済み") {
		t.Fatalf("反映済みはやめられない: %q", r.Data.Content)
	}
}
