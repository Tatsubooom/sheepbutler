package dispatch

import (
	"fmt"
	"net/mail"
	"sort"
	"strings"
	"time"

	"github.com/Tatsubooom/sheepbutler/internal/calendar"
	"github.com/Tatsubooom/sheepbutler/internal/model"
	"github.com/Tatsubooom/sheepbutler/internal/selection"
)

var weekdays = []string{"日", "月", "火", "水", "木", "金", "土"}

// clock は "10/06(火) 23:59" の形にする。
func clock(t time.Time, loc *time.Location) string {
	t = t.In(loc)
	return fmt.Sprintf("%02d/%02d(%s) %02d:%02d", t.Month(), t.Day(), weekdays[t.Weekday()], t.Hour(), t.Minute())
}

// remaining は「あと2日」「あと5時間」の形にする。
func remaining(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("あと%d分", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("あと%d時間", int(d.Hours()))
	}
	return fmt.Sprintf("あと%d日", int(d.Hours()/24))
}

// title はメールの見出し。LLM の要約があればそれを、無ければ件名を使う。改行は除く。
func title(m *model.Message) string {
	s := m.Subject
	if m.Extracted != nil && m.Extracted.Summary != "" {
		s = m.Extracted.Summary
	}
	if strings.TrimSpace(s) == "" {
		s = "（件名なし）"
	}
	return oneLine(s, 60)
}

// sender は差出人の表示名（無ければアドレス）。
func sender(m *model.Message) string {
	if a, err := mail.ParseAddress(m.From); err == nil {
		if a.Name != "" {
			return oneLine(a.Name, 30)
		}
		return a.Address
	}
	return oneLine(m.From, 40)
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

func isPhishing(m *model.Message) bool { return m.Extracted != nil && m.Extracted.PhishingSuspect }

func deadlineOf(m *model.Message) (time.Time, bool) {
	if m.Extracted == nil {
		return time.Time{}, false
	}
	return m.Extracted.DeadlineTime()
}

const phishingWarning = "⚠️ 詐欺の疑いがあります。メール内のリンクや添付ファイルは開かないでください。"

// formatUrgent は即時通知の本文を作る。読みやすさのため、見出し・差出人と締切・選考の動き・リンクだけにする。
func formatUrgent(m *model.Message, now time.Time, loc *time.Location, link string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🔴 %s\n", title(m))
	if isPhishing(m) {
		b.WriteString(phishingWarning + "\n")
	}
	meta := sender(m)
	if dl, ok := deadlineOf(m); ok && dl.After(now) {
		meta += fmt.Sprintf("｜締切 %s（%s）", clock(dl, loc), remaining(dl.Sub(now)))
	}
	b.WriteString(meta + "\n")
	if m.SelectionChange != "" {
		fmt.Fprintf(&b, "選考: %s\n", m.SelectionChange)
	}
	if link != "" {
		b.WriteString(link)
	}
	return strings.TrimRight(b.String(), "\n")
}

// formatReminders は締切リマインドの本文を作る。
func formatReminders(ms []*model.Message, now time.Time, loc *time.Location) string {
	sortByDeadline(ms)
	var b strings.Builder
	b.WriteString("⏰ 締切リマインド\n")
	for _, m := range ms {
		dl, _ := deadlineOf(m)
		fmt.Fprintf(&b, "• %s（%s）— %s\n", clock(dl, loc), remaining(dl.Sub(now)), title(m))
	}
	return strings.TrimRight(b.String(), "\n")
}

// digestInput はまとめ報告の材料。
type digestInput struct {
	Slot          time.Time
	EventsDay     time.Time // 予定を載せる日
	Events        []calendar.Event
	EventsErr     error
	Deadlines     []*model.Message // 締切が近いメール
	Items         []*model.Message // まとめ待ちのメール
	RecordedCount int              // 記録のみにしたメールの件数
	MaxItems      int
	Selection     *selection.Overview // 選考状況（nil なら欄を省く）
}

// formatDigest はまとめ報告の本文を作る。
func formatDigest(in digestInput, now time.Time, loc *time.Location) string {
	var b strings.Builder
	part := "夜"
	if in.Slot.In(loc).Hour() < 12 {
		part = "朝"
	}
	s := in.Slot.In(loc)
	fmt.Fprintf(&b, "🐑 まとめ報告（%02d/%02d %s）\n", s.Month(), s.Day(), part)

	day := in.EventsDay.In(loc)
	label := "今日"
	if day.Day() != s.Day() {
		label = "明日"
	}
	fmt.Fprintf(&b, "\n📅 %sの予定（%02d/%02d %s）\n", label, day.Month(), day.Day(), weekdays[day.Weekday()])
	switch {
	case in.EventsErr != nil:
		b.WriteString("（予定を取得できませんでした）\n")
	case len(in.Events) == 0:
		b.WriteString("予定はありません\n")
	default:
		for _, e := range in.Events {
			when := "終日"
			if !e.AllDay {
				when = fmt.Sprintf("%s〜%s", e.Start.In(loc).Format("15:04"), e.End.In(loc).Format("15:04"))
			}
			line := fmt.Sprintf("• %s %s", when, oneLine(e.Summary, 40))
			if e.Location != "" {
				line += "（" + oneLine(e.Location, 20) + "）"
			}
			b.WriteString(line + "\n")
		}
	}

	if len(in.Deadlines) > 0 {
		b.WriteString("\n⏰ 近い締切\n")
		sortByDeadline(in.Deadlines)
		for _, m := range in.Deadlines {
			dl, _ := deadlineOf(m)
			fmt.Fprintf(&b, "• %s（%s）— %s\n", clock(dl, loc), remaining(dl.Sub(now)), title(m))
		}
	}

	writeSelection(&b, in.Selection, now, loc)

	// 近い締切の欄に載せたメールは、確認してほしいメールの欄には重ねて載せない。
	shown := map[string]bool{}
	for _, m := range in.Deadlines {
		shown[m.ID] = true
	}
	var items []*model.Message
	for _, m := range in.Items {
		if m.ID == "" || !shown[m.ID] {
			items = append(items, m)
		}
	}
	if len(items) > 0 {
		sort.SliceStable(items, func(i, j int) bool { return score(items[i]) > score(items[j]) })
		fmt.Fprintf(&b, "\n📬 確認してほしいメール（%d通）\n", len(items))
		for i, m := range items {
			if i == in.MaxItems {
				fmt.Fprintf(&b, "…ほか %d 通\n", len(items)-in.MaxItems)
				break
			}
			mark := "•"
			if m.Triage != nil && m.Triage.Tier == model.TierUrgent {
				mark = "🔴" // 通知しない時間帯などで即時通知を見送ったもの
			}
			if isPhishing(m) {
				mark = "⚠️"
			}
			fmt.Fprintf(&b, "%s %s（%s）\n", mark, title(m), sender(m))
		}
	} else if len(in.Deadlines) == 0 {
		b.WriteString("\n📬 確認してほしいメールはありません\n")
	}

	if in.RecordedCount > 0 {
		fmt.Fprintf(&b, "\n🗂 記録のみ: %d通（広告や通知など）\n", in.RecordedCount)
	}
	return strings.TrimRight(b.String(), "\n")
}

// writeSelection は選考状況の欄を書く。選考中・変化・要確認・連絡なしのどれも無ければ省く。
func writeSelection(b *strings.Builder, o *selection.Overview, now time.Time, loc *time.Location) {
	if o == nil || len(o.Active)+len(o.Changed)+len(o.Review)+len(o.Stale) == 0 {
		return
	}
	b.WriteString("\n📋 選考状況\n")
	for _, c := range o.Changed {
		fmt.Fprintf(b, "🆕 %s %s\n", oneLine(c.Name, 30), c.LastChange)
	}
	for _, c := range o.Review {
		note := "メールの判定だけで更新したので確認してください"
		if c.NeedsReview && c.ReviewNote != "" {
			note = c.ReviewNote
		}
		fmt.Fprintf(b, "❓ %s %s — %s\n", oneLine(c.Name, 30), c.Label(), note)
	}
	for _, c := range o.Active {
		line := fmt.Sprintf("• %s %s", oneLine(c.Name, 30), c.Label())
		if c.NextAction.Label != "" {
			line += "｜次: " + c.NextAction.Label
			if !c.NextAction.Due.IsZero() && c.NextAction.Due.After(now) {
				line += "（" + clock(c.NextAction.Due, loc) + "まで）"
			}
		}
		b.WriteString(line + "\n")
	}
	for _, c := range o.Stale {
		days := int(now.Sub(c.LastContactAt).Hours() / 24)
		fmt.Fprintf(b, "⌛ %s %s — %d日間連絡がありません\n", oneLine(c.Name, 30), c.Label(), days)
	}
}

func score(m *model.Message) float64 {
	if m.Triage == nil {
		return 0
	}
	if m.Triage.Tier == model.TierUrgent {
		return m.Triage.Score + 10 // 見送った即時通知は先頭に
	}
	return m.Triage.Score
}

func sortByDeadline(ms []*model.Message) {
	sort.SliceStable(ms, func(i, j int) bool {
		a, _ := deadlineOf(ms[i])
		b, _ := deadlineOf(ms[j])
		return a.Before(b)
	})
}

// Clock・Remaining・Title・DeadlineOf は、チャットの応答で通知と同じ書式を使うために公開する。
func Clock(t time.Time, loc *time.Location) string  { return clock(t, loc) }
func Remaining(d time.Duration) string              { return remaining(d) }
func Title(m *model.Message) string                 { return title(m) }
func DeadlineOf(m *model.Message) (time.Time, bool) { return deadlineOf(m) }
