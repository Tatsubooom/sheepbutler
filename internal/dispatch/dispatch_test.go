package dispatch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Tatsubooom/sheepbutler/internal/calendar"
	"github.com/Tatsubooom/sheepbutler/internal/config"
	"github.com/Tatsubooom/sheepbutler/internal/llm"
	"github.com/Tatsubooom/sheepbutler/internal/model"
)

var jst = time.FixedZone("JST", 9*3600)

type fakeStore struct {
	msgs          map[string]*model.Message
	notifications []model.Notification
	lastDigest    time.Time
}

func newStore(ms ...*model.Message) *fakeStore {
	s := &fakeStore{msgs: map[string]*model.Message{}}
	for _, m := range ms {
		s.msgs[m.ID] = m
	}
	return s
}

func (s *fakeStore) ListByNotify(_ context.Context, st model.NotifyState, limit int) ([]*model.Message, error) {
	var out []*model.Message
	for _, id := range sortedKeys(s.msgs) {
		if m := s.msgs[id]; m.Notify == st && len(out) < limit {
			out = append(out, m)
		}
	}
	return out, nil
}
func (s *fakeStore) ListReceivedSince(_ context.Context, since time.Time) ([]*model.Message, error) {
	var out []*model.Message
	for _, id := range sortedKeys(s.msgs) {
		if m := s.msgs[id]; !m.ReceivedAt.Before(since) {
			out = append(out, m)
		}
	}
	return out, nil
}
func (s *fakeStore) SetNotify(_ context.Context, ids []string, st model.NotifyState, at time.Time) error {
	for _, id := range ids {
		s.msgs[id].Notify = st
		if !at.IsZero() {
			s.msgs[id].NotifiedAt = at
		}
	}
	return nil
}
func (s *fakeStore) AddReminded(_ context.Context, id string, hours []int) error {
	s.msgs[id].Reminded = append(s.msgs[id].Reminded, hours...)
	return nil
}
func (s *fakeStore) ListNotificationsSince(_ context.Context, since time.Time) ([]model.Notification, error) {
	var out []model.Notification
	for _, n := range s.notifications {
		if !n.At.Before(since) {
			out = append(out, n)
		}
	}
	return out, nil
}
func (s *fakeStore) AddNotification(_ context.Context, n model.Notification) error {
	s.notifications = append(s.notifications, n)
	return nil
}
func (s *fakeStore) LastDigest(context.Context) (time.Time, error)      { return s.lastDigest, nil }
func (s *fakeStore) SetLastDigest(_ context.Context, t time.Time) error { s.lastDigest = t; return nil }

func sortedKeys(m map[string]*model.Message) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

type fakeNotifier struct {
	name string
	sent []string
	err  error
}

func (f *fakeNotifier) Name() string { return f.name }
func (f *fakeNotifier) Send(_ context.Context, text string) error {
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, text)
	return nil
}

type fakeCalendar struct{ events []calendar.Event }

func (f *fakeCalendar) Events(context.Context, time.Time, time.Time) ([]calendar.Event, error) {
	return f.events, nil
}

func at(day, hour, min int) time.Time { return time.Date(2026, 10, day, hour, min, 0, 0, jst) }

func newDispatcher(s *fakeStore, n *fakeNotifier, now time.Time) *Dispatcher {
	return &Dispatcher{
		Store: s, Notifier: n, Calendar: &fakeCalendar{}, Config: config.Default(), Loc: jst,
		Retention: 90 * 24 * time.Hour, Link: func(id string) string { return "https://mail.example/" + id },
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return now },
	}
}

func urgentMsg(id string) *model.Message {
	return &model.Message{ID: id, Subject: "面接日程のご連絡", From: "人事 <hr@example.co.jp>", Source: "大学",
		ReceivedAt: at(4, 10, 0), Notify: model.NotifyUrgent,
		Extracted: &llm.Extraction{Summary: "一次面接の日程調整", Deadline: at(6, 23, 59).Format(time.RFC3339)},
		Triage:    &model.TriageResult{Tier: model.TierUrgent, Reasons: []string{"選考の個別連絡"}, At: at(4, 10, 0)}}
}

func TestSendUrgent(t *testing.T) {
	s, n := newStore(urgentMsg("a")), &fakeNotifier{name: "Discord"}
	sent, err := newDispatcher(s, n, at(4, 10, 1)).SendUrgent(context.Background())
	if err != nil || sent != 1 {
		t.Fatalf("sent=%d err=%v", sent, err)
	}
	if s.msgs["a"].Notify != model.NotifyDone || len(s.notifications) != 1 {
		t.Fatalf("送ったら済みにして記録する: %+v", s.msgs["a"])
	}
	for _, want := range []string{"🔴 即時｜一次面接の日程調整", "人事（大学）", "10/06(火) 23:59（あと2日）", "選考の個別連絡", "https://mail.example/a"} {
		if !strings.Contains(n.sent[0], want) {
			t.Errorf("本文に %q が無い:\n%s", want, n.sent[0])
		}
	}
}

func TestSendUrgentDefersInQuietHours(t *testing.T) {
	s, n := newStore(urgentMsg("a")), &fakeNotifier{name: "Discord"}
	newDispatcher(s, n, at(5, 2, 0)).SendUrgent(context.Background())
	if len(n.sent) != 0 || s.msgs["a"].Notify != model.NotifyDigest {
		t.Fatalf("通知しない時間帯はまとめ報告に回す: sent=%d notify=%s", len(n.sent), s.msgs["a"].Notify)
	}
}

func TestQuietHoursAcrossMidnight(t *testing.T) {
	d := newDispatcher(newStore(), &fakeNotifier{}, at(4, 0, 0))
	d.Config.Notify.QuietHours = []string{"23:00", "07:00"}
	for _, c := range []struct {
		t    time.Time
		want bool
	}{{at(4, 23, 30), true}, {at(4, 3, 0), true}, {at(4, 7, 0), false}, {at(4, 12, 0), false}} {
		if got := d.inQuiet(c.t); got != c.want {
			t.Errorf("%v: got %v", c.t.Format("15:04"), got)
		}
	}
}

func TestSendUrgentRateLimit(t *testing.T) {
	s := newStore(urgentMsg("a"), urgentMsg("b"), urgentMsg("c"))
	n := &fakeNotifier{name: "Discord"}
	d := newDispatcher(s, n, at(4, 12, 0))
	d.Config.Notify.MaxUrgentPerHour = 2
	sent, _ := d.SendUrgent(context.Background())
	if sent != 2 || s.msgs["c"].Notify != model.NotifyDigest {
		t.Fatalf("上限を超えた分はまとめ報告に回す: sent=%d c=%s", sent, s.msgs["c"].Notify)
	}
}

func TestSendUrgentLINEMonthlyReserve(t *testing.T) {
	s := newStore(urgentMsg("a"))
	for i := 0; i < 160; i++ { // 上限 200 の 80%
		s.notifications = append(s.notifications, model.Notification{At: at(2, 12, 0), Kind: kindDigest, Channel: "LINE"})
	}
	n := &fakeNotifier{name: "LINE"}
	newDispatcher(s, n, at(4, 12, 0)).SendUrgent(context.Background())
	if len(n.sent) != 0 || s.msgs["a"].Notify != model.NotifyDigest {
		t.Fatal("LINE の月間上限に近づいたら即時通知をやめ、まとめ報告だけにする")
	}
}

func TestSendUrgentKeepsStateOnFailure(t *testing.T) {
	s, n := newStore(urgentMsg("a")), &fakeNotifier{name: "Discord", err: errors.New("503")}
	if _, err := newDispatcher(s, n, at(4, 12, 0)).SendUrgent(context.Background()); err == nil {
		t.Fatal("送信に失敗したらエラーを返す")
	}
	if s.msgs["a"].Notify != model.NotifyUrgent {
		t.Fatal("失敗したら即時通知待ちのまま（次の Tick で再送）")
	}
}

func TestPhishingWarning(t *testing.T) {
	m := urgentMsg("a")
	m.Extracted.PhishingSuspect = true
	if !strings.Contains(formatUrgent(m, at(4, 12, 0), jst, ""), "詐欺の疑い") {
		t.Fatal("詐欺の疑いを警告する")
	}
}

func digestMsg(id string, score float64, received time.Time) *model.Message {
	return &model.Message{ID: id, Subject: "件名" + id, From: "a@example.com", ReceivedAt: received, Notify: model.NotifyDigest,
		Triage: &model.TriageResult{Tier: model.TierDigest, Score: score, At: received}}
}

func TestDigestSchedule(t *testing.T) {
	d := newDispatcher(newStore(), &fakeNotifier{}, at(4, 7, 0))
	tests := []struct {
		now, last time.Time
		want      time.Time
	}{
		{at(4, 6, 59), time.Time{}, time.Time{}},   // まだ
		{at(4, 7, 5), time.Time{}, at(4, 7, 0)},    // 朝の報告
		{at(4, 7, 20), at(4, 7, 0), time.Time{}},   // 送信済み
		{at(4, 10, 0), time.Time{}, time.Time{}},   // 2時間以上遅れたら送らない
		{at(4, 21, 10), at(4, 7, 0), at(4, 21, 0)}, // 夜の報告
	}
	for _, tt := range tests {
		got, ok := d.dueSlot(tt.now, tt.last)
		if !got.Equal(tt.want) || ok == tt.want.IsZero() {
			t.Errorf("now=%s last=%s: got %v", tt.now.Format("15:04"), tt.last.Format("15:04"), got)
		}
	}
}

func TestSendDigest(t *testing.T) {
	deferred := urgentMsg("u")
	deferred.Notify = model.NotifyDigest // 夜中に届いて見送った即時通知
	rec := &model.Message{ID: "r", ReceivedAt: at(4, 1, 0), Notify: model.NotifyNone, Triage: &model.TriageResult{Tier: model.TierRecord}}
	s := newStore(digestMsg("a", 0.4, at(4, 1, 0)), digestMsg("b", 0.6, at(4, 2, 0)), deferred, rec)
	n := &fakeNotifier{name: "Discord"}
	d := newDispatcher(s, n, at(5, 7, 1))
	d.Calendar = &fakeCalendar{events: []calendar.Event{
		{Summary: "情報理論", Start: at(5, 9, 0), End: at(5, 10, 30)},
		{Summary: "学園祭", AllDay: true, Start: at(5, 0, 0)},
	}}

	sent, err := d.SendDigestIfDue(context.Background())
	if err != nil || !sent {
		t.Fatalf("sent=%v err=%v", sent, err)
	}
	body := n.sent[0]
	for _, want := range []string{"🐑 まとめ報告（10/05 朝）", "今日の予定（10/05 月）", "09:00〜10:30 情報理論", "終日 学園祭",
		"近い締切", "10/06(火) 23:59", "確認してほしいメール（3通）", "記録のみ: 1通"} {
		if !strings.Contains(body, want) {
			t.Errorf("本文に %q が無い:\n%s", want, body)
		}
	}
	// 見送った即時通知が先頭、残りは点数の高い順
	iu, ib, ia := strings.Index(body, "🔴"), strings.Index(body, "件名b"), strings.Index(body, "件名a")
	if !(iu < ib && ib < ia) {
		t.Errorf("並び順が違う:\n%s", body)
	}
	for _, id := range []string{"a", "b", "u", "r"} {
		if s.msgs[id].Notify != model.NotifyDone {
			t.Errorf("%s を済みにする", id)
		}
	}
	if again, _ := d.SendDigestIfDue(context.Background()); again {
		t.Fatal("同じ時刻の報告は1回だけ")
	}
}

func TestEveningDigestShowsTomorrow(t *testing.T) {
	n := &fakeNotifier{name: "Discord"}
	newDispatcher(newStore(), n, at(4, 21, 0)).SendDigestIfDue(context.Background())
	if !strings.Contains(n.sent[0], "明日の予定（10/05 月）") || !strings.Contains(n.sent[0], "予定はありません") {
		t.Fatalf("夜の報告は明日の予定:\n%s", n.sent[0])
	}
}

func TestReminders(t *testing.T) {
	m := digestMsg("a", 0.5, at(1, 9, 0))
	m.Notify = model.NotifyDone
	m.Extracted = &llm.Extraction{Summary: "ES提出", Deadline: at(6, 23, 59).Format(time.RFC3339)} // 判定時は残り5日
	s := newStore(m)
	n := &fakeNotifier{name: "Discord"}

	newDispatcher(s, n, at(3, 12, 0)).SendReminders(context.Background()) // 残り約84時間: まだ
	if len(n.sent) != 0 {
		t.Fatal("72時間前より前は送らない")
	}
	newDispatcher(s, n, at(4, 12, 0)).SendReminders(context.Background()) // 残り約60時間
	newDispatcher(s, n, at(4, 12, 15)).SendReminders(context.Background())
	if len(n.sent) != 1 || !strings.Contains(n.sent[0], "ES提出") || !slices.Contains(m.Reminded, 72) {
		t.Fatalf("72時間前のリマインドを1回だけ送る: %v %v", n.sent, m.Reminded)
	}
	newDispatcher(s, n, at(6, 2, 0)).SendReminders(context.Background()) // 通知しない時間帯
	if len(n.sent) != 1 {
		t.Fatal("通知しない時間帯は待つ")
	}
	newDispatcher(s, n, at(6, 8, 0)).SendReminders(context.Background()) // 残り約16時間
	if len(n.sent) != 2 || !slices.Contains(m.Reminded, 24) {
		t.Fatalf("24時間前のリマインド: %v", m.Reminded)
	}
}

func TestReminderSkippedWhenDeadlineAlreadyNearAtArrival(t *testing.T) {
	m := urgentMsg("a") // 判定時（10/04 10:00）に締切まで約62時間
	m.Notify = model.NotifyDone
	s := newStore(m)
	n := &fakeNotifier{name: "Discord"}
	newDispatcher(s, n, at(4, 12, 0)).SendReminders(context.Background())
	if len(n.sent) != 0 || !slices.Contains(m.Reminded, 72) {
		t.Fatalf("届いた時点で72時間を切っていたら、72時間前のリマインドは即時通知で伝え済みとして送らない: %v", n.sent)
	}
}
