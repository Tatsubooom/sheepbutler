// Package dispatch は判定済みのメールを Discord / LINE に知らせる。
//
//   - 即時通知: 判定の直後に送る。通知しない時間帯や回数の上限に当たったら、次のまとめ報告に回す
//   - 締切リマインド: 締切の reminder_hours 時間前に送る（通知しない時間帯は待つ）
//   - まとめ報告: digest.times の時刻に、予定・近い締切・確認してほしいメールをまとめて送る
//
// Tick を15分おきに呼ぶと、送り損ねた即時通知・リマインド・まとめ報告をまとめて処理する。
package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/Tatsubooom/sheepbutler/internal/calendar"
	"github.com/Tatsubooom/sheepbutler/internal/config"
	"github.com/Tatsubooom/sheepbutler/internal/model"
	"github.com/Tatsubooom/sheepbutler/internal/notify"
)

// Store は通知に必要な保存先の操作。
type Store interface {
	ListByNotify(ctx context.Context, state model.NotifyState, limit int) ([]*model.Message, error)
	// ListReceivedSince は since 以降に受信したメールを返す（締切の確認に使う）。
	ListReceivedSince(ctx context.Context, since time.Time) ([]*model.Message, error)
	SetNotify(ctx context.Context, ids []string, state model.NotifyState, at time.Time) error
	AddReminded(ctx context.Context, id string, hours []int) error
	ListNotificationsSince(ctx context.Context, since time.Time) ([]model.Notification, error)
	AddNotification(ctx context.Context, n model.Notification) error
	LastDigest(ctx context.Context) (time.Time, error)
	SetLastDigest(ctx context.Context, slot time.Time) error
}

// Calendar は予定を読む。nil なら予定の欄を省く。
type Calendar interface {
	Events(ctx context.Context, from, to time.Time) ([]calendar.Event, error)
}

type Dispatcher struct {
	Store     Store
	Notifier  notify.Notifier
	Calendar  Calendar
	Config    config.Config
	Loc       *time.Location
	Retention time.Duration          // 送信記録の保存期間
	Link      func(id string) string // メールを開く URL（専用 Gmail のスレッド）
	Log       *slog.Logger
	Now       func() time.Time
}

const (
	kindUrgent   = "urgent"
	kindDigest   = "digest"
	kindReminder = "reminder"

	// digestGrace を過ぎたまとめ報告は送らない（停止明けに古い報告が届くのを防ぐ）。
	digestGrace = 2 * time.Hour
	// lineReserve は LINE の月間上限のうち、まとめ報告のために残しておく割合。
	lineReserve = 0.8
)

func (d *Dispatcher) now() time.Time {
	if d.Now != nil {
		return d.Now().In(d.Loc)
	}
	return time.Now().In(d.Loc)
}

// Tick は即時通知・リマインド・まとめ報告を順に処理する。1つが失敗しても残りは続ける。
func (d *Dispatcher) Tick(ctx context.Context) error {
	_, errU := d.SendUrgent(ctx)
	errR := d.SendReminders(ctx)
	_, errD := d.SendDigestIfDue(ctx)
	return errors.Join(errU, errR, errD)
}

// inQuiet は通知しない時間帯かを返す。日をまたぐ指定（例: 23:00〜07:00）にも対応する。
func (d *Dispatcher) inQuiet(t time.Time) bool {
	q := d.Config.Notify.QuietHours
	if len(q) != 2 {
		return false
	}
	start, err1 := config.ParseClock(q[0])
	end, err2 := config.ParseClock(q[1])
	if err1 != nil || err2 != nil || start == end {
		return false
	}
	m := t.In(d.Loc).Hour()*60 + t.In(d.Loc).Minute()
	if start < end {
		return start <= m && m < end
	}
	return m >= start || m < end
}

// SendUrgent は即時通知を待っているメールを送る。送れないものはまとめ報告に回す。
func (d *Dispatcher) SendUrgent(ctx context.Context) (int, error) {
	msgs, err := d.Store.ListByNotify(ctx, model.NotifyUrgent, 20)
	if err != nil || len(msgs) == 0 {
		return 0, err
	}
	now := d.now()

	defer_ := func(ms []*model.Message, why string) error {
		d.Log.Info("即時通知をまとめ報告に回しました", "count", len(ms), "reason", why)
		return d.Store.SetNotify(ctx, ids(ms), model.NotifyDigest, time.Time{})
	}
	if d.inQuiet(now) {
		return 0, defer_(msgs, "通知しない時間帯")
	}

	recent, err := d.Store.ListNotificationsSince(ctx, startOfMonth(now))
	if err != nil {
		return 0, err
	}
	hour, day, month := 0, 0, 0
	for _, n := range recent {
		if n.Kind == kindUrgent && n.At.After(now.Add(-time.Hour)) {
			hour++
		}
		if n.Kind == kindUrgent && n.At.After(now.Add(-24*time.Hour)) {
			day++
		}
		if n.Channel == d.Notifier.Name() {
			month++
		}
	}

	sent := 0
	for i, m := range msgs {
		cfg := d.Config.Notify
		switch {
		case cfg.MaxUrgentPerHour > 0 && hour >= cfg.MaxUrgentPerHour,
			cfg.MaxUrgentPerDay > 0 && day >= cfg.MaxUrgentPerDay:
			return sent, defer_(msgs[i:], "即時通知の上限")
		case d.Notifier.Name() == "LINE" && cfg.LineMonthlyLimit > 0 && float64(month) >= lineReserve*float64(cfg.LineMonthlyLimit):
			return sent, defer_(msgs[i:], "LINE の月間送信数に近い")
		}
		link := ""
		if d.Link != nil {
			link = d.Link(m.ID)
		}
		if err := d.send(ctx, kindUrgent, formatUrgent(m, now, d.Loc, link), []string{m.ID}); err != nil {
			return sent, err // 状態は urgent のまま。次の Tick で再送する
		}
		if err := d.Store.SetNotify(ctx, []string{m.ID}, model.NotifyDone, now); err != nil {
			return sent, err
		}
		sent++
		hour++
		day++
		month++
	}
	return sent, nil
}

// SendReminders は締切が reminder_hours 以内に入ったメールをまとめて1通で知らせる。
func (d *Dispatcher) SendReminders(ctx context.Context) error {
	now := d.now()
	if d.inQuiet(now) || len(d.Config.Notify.ReminderHours) == 0 {
		return nil
	}
	msgs, err := d.Store.ListReceivedSince(ctx, now.Add(-60*24*time.Hour))
	if err != nil {
		return err
	}
	var due []*model.Message
	newly := map[string][]int{}
	for _, m := range msgs {
		if m.Triage == nil || m.Triage.Tier == model.TierRecord {
			continue
		}
		dl, ok := deadlineOf(m)
		if !ok || !dl.After(now) {
			continue
		}
		left := dl.Sub(now)
		var hit []int
		for _, h := range d.Config.Notify.ReminderHours {
			if left <= time.Duration(h)*time.Hour && !slices.Contains(m.Reminded, h) {
				hit = append(hit, h)
			}
		}
		// 届いた時点で既に締切が近いメール（即時通知やまとめ報告で締切を伝え済み）は、
		// その時点で当てはまるリマインドを済みにして、同じ内容を重ねて送らない。
		if len(hit) > 0 {
			due = append(due, m)
			newly[m.ID] = hit
		}
	}
	if len(due) == 0 {
		return nil
	}
	var send []*model.Message
	for _, m := range due {
		if m.Triage.At.IsZero() || !alreadyCovered(m, newly[m.ID], d.Config.Notify.ReminderHours) {
			send = append(send, m)
		}
	}
	if len(send) > 0 {
		if err := d.send(ctx, kindReminder, formatReminders(send, now, d.Loc), ids(send)); err != nil {
			return err
		}
	}
	for _, m := range due {
		if err := d.Store.AddReminded(ctx, m.ID, newly[m.ID]); err != nil {
			return err
		}
	}
	return nil
}

// alreadyCovered は、判定した時点で既にすべてのリマインド時刻を過ぎていたか（＝初回の通知で締切を伝え済みか）を返す。
func alreadyCovered(m *model.Message, hit []int, all []int) bool {
	dl, _ := deadlineOf(m)
	leftAtTriage := dl.Sub(m.Triage.At)
	for _, h := range hit {
		if leftAtTriage > time.Duration(h)*time.Hour {
			return false // 判定の後にこのリマインド時刻に入った
		}
	}
	return true
}

// dueSlot は今送るべきまとめ報告の時刻を返す。無ければ ok=false。
func (d *Dispatcher) dueSlot(now, last time.Time) (time.Time, bool) {
	var best time.Time
	for _, s := range d.Config.Digest.Times {
		min, err := config.ParseClock(s)
		if err != nil {
			continue
		}
		y, mo, da := now.Date()
		slot := time.Date(y, mo, da, min/60, min%60, 0, 0, d.Loc)
		if !slot.After(now) && now.Sub(slot) < digestGrace && slot.After(last) && slot.After(best) {
			best = slot
		}
	}
	return best, !best.IsZero()
}

// SendDigestIfDue はまとめ報告の時刻になっていれば送る。
func (d *Dispatcher) SendDigestIfDue(ctx context.Context) (bool, error) {
	now := d.now()
	last, err := d.Store.LastDigest(ctx)
	if err != nil {
		return false, err
	}
	slot, ok := d.dueSlot(now, last)
	if !ok {
		return false, nil
	}

	in := digestInput{Slot: slot, MaxItems: d.Config.Digest.MaxItems}
	// 正午より前の報告は今日の予定、それ以降は明日の予定を載せる。
	y, mo, da := slot.Date()
	in.EventsDay = time.Date(y, mo, da, 0, 0, 0, 0, d.Loc)
	if slot.Hour() >= 12 {
		in.EventsDay = in.EventsDay.AddDate(0, 0, 1)
	}
	if d.Calendar != nil {
		in.Events, in.EventsErr = d.Calendar.Events(ctx, in.EventsDay, in.EventsDay.AddDate(0, 0, 1))
		if in.EventsErr != nil {
			d.Log.Warn("予定を取得できません", "err", in.EventsErr)
		}
	}

	recent, err := d.Store.ListReceivedSince(ctx, now.Add(-60*24*time.Hour))
	if err != nil {
		return false, err
	}
	horizon := now.Add(time.Duration(d.Config.Digest.DeadlineDays) * 24 * time.Hour)
	for _, m := range recent {
		if dl, ok := deadlineOf(m); ok && m.Triage != nil && m.Triage.Tier != model.TierRecord && dl.After(now) && !dl.After(horizon) {
			in.Deadlines = append(in.Deadlines, m)
		}
	}
	if in.Items, err = d.Store.ListByNotify(ctx, model.NotifyDigest, 200); err != nil {
		return false, err
	}
	recorded, err := d.Store.ListByNotify(ctx, model.NotifyNone, 500)
	if err != nil {
		return false, err
	}
	in.RecordedCount = len(recorded)

	if err := d.send(ctx, kindDigest, formatDigest(in, now, d.Loc), ids(in.Items)); err != nil {
		return false, err
	}
	// 送った後に状態を進める。途中で失敗しても、次の報告で同じメールがもう一度載るだけで、取りこぼしはない。
	if err := d.Store.SetLastDigest(ctx, slot); err != nil {
		return true, err
	}
	if err := d.Store.SetNotify(ctx, ids(in.Items), model.NotifyDone, now); err != nil {
		return true, err
	}
	return true, d.Store.SetNotify(ctx, ids(recorded), model.NotifyDone, now)
}

func (d *Dispatcher) send(ctx context.Context, kind, text string, msgIDs []string) error {
	if err := d.Notifier.Send(ctx, text); err != nil {
		return fmt.Errorf("%s への送信（%s）: %w", d.Notifier.Name(), kind, err)
	}
	now := d.now()
	if err := d.Store.AddNotification(ctx, model.Notification{
		At: now, Kind: kind, Channel: d.Notifier.Name(), MessageIDs: msgIDs, ExpiresAt: now.Add(d.Retention),
	}); err != nil {
		d.Log.Error("送信記録の保存に失敗", "err", err)
	}
	d.Log.Info("通知しました", "kind", kind, "channel", d.Notifier.Name(), "messages", len(msgIDs))
	return nil
}

func ids(ms []*model.Message) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return out
}

func startOfMonth(t time.Time) time.Time {
	y, m, _ := t.Date()
	return time.Date(y, m, 1, 0, 0, 0, 0, t.Location())
}
