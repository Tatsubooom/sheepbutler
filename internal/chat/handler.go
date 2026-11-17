// Package chat は Discord のスラッシュコマンドとボタンに応答する（設計書の M5）。
//
// 操作できるのは Bot の持ち主（OwnerID）だけ。応答は本人にだけ見える形（ephemeral）で返す。
package chat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/Tatsubooom/sheepbutler/internal/config"
	"github.com/Tatsubooom/sheepbutler/internal/discord"
	"github.com/Tatsubooom/sheepbutler/internal/dispatch"
	"github.com/Tatsubooom/sheepbutler/internal/model"
	"github.com/Tatsubooom/sheepbutler/internal/selection"
)

// Store はチャットが使う保存先の操作。
type Store interface {
	GetMessage(ctx context.Context, id string) (*model.Message, error)
	ListByNotify(ctx context.Context, state model.NotifyState, limit int) ([]*model.Message, error)
	ListReceivedSince(ctx context.Context, since time.Time) ([]*model.Message, error)
	PausedUntil(ctx context.Context) (time.Time, error)
	SetPausedUntil(ctx context.Context, until time.Time) error
}

// Tracker は選考状況の操作。
type Tracker interface {
	Overview(ctx context.Context, since time.Time) (selection.Overview, error)
	Confirm(ctx context.Context, companyID, historyID string) (*model.Company, bool, error)
	Undo(ctx context.Context, companyID, historyID string) (*model.Company, bool, error)
	SetManual(ctx context.Context, name string, stage model.Stage, round int, historyID string) (*model.Company, bool, error)
}

type Handler struct {
	Store     Store
	Tracker   Tracker
	Proposals ProposalStore     // 変更案の反映（/ask で作られたもの）
	Publisher Publisher         // /ask の質問を本体に渡す
	Config    config.Config     // 設定の変更案を検証するための元の設定
	Calendar  dispatch.Calendar // nil なら予定は表示しない
	OwnerID   string            // 操作を許す Discord ユーザー
	Loc       *time.Location
	Log       *slog.Logger
	Now       func() time.Time
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now().In(h.Loc)
	}
	return time.Now().In(h.Loc)
}

// Handle は1つの操作に応答する。Discord は3秒以内の応答を求めるので、重い処理はしない。
func (h *Handler) Handle(ctx context.Context, in discord.Interaction) discord.Response {
	if in.Type == discord.TypePing {
		return discord.Response{Type: discord.RespPong}
	}
	if h.OwnerID == "" || in.UserID() != h.OwnerID {
		return discord.Message("この Bot は持ち主だけが操作できます。", true)
	}

	var text string
	var err error
	switch in.Type {
	case discord.TypeApplicationCmd:
		switch in.Data.Name {
		case "status":
			text, err = h.status(ctx)
		case "deadlines":
			text, err = h.deadlines(ctx)
		case "today":
			text, err = h.today(ctx)
		case "pause":
			hours, ok := in.NumberOption("hours")
			if !ok {
				hours = 24
			}
			text, err = h.pause(ctx, hours)
		case "resume":
			text, err = h.resume(ctx)
		case "ask":
			return h.ask(ctx, in)
		default:
			text = "知らないコマンドです: /" + in.Data.Name
		}
	case discord.TypeMessageComponent:
		return h.button(ctx, in)
	}
	if err != nil {
		h.Log.Error("コマンドの処理に失敗", "command", in.Data.Name, "err", err)
		return discord.Message("処理に失敗しました。時間をおいてもう一度試してください。", true)
	}
	return discord.Message(text, true)
}

func (h *Handler) status(ctx context.Context) (string, error) {
	o, err := h.Tracker.Overview(ctx, time.Time{})
	if err != nil {
		return "", err
	}
	if len(o.Active)+len(o.Review) == 0 {
		return "📋 選考中の企業はまだありません。選考のメールが届くと自動で追加されます。", nil
	}
	now := h.now()
	var b strings.Builder
	b.WriteString("📋 選考状況\n")
	for _, c := range o.Review {
		note := "メールの判定だけで更新しました"
		if c.NeedsReview && c.ReviewNote != "" {
			note = c.ReviewNote
		}
		fmt.Fprintf(&b, "❓ %s %s — %s\n", c.Name, c.Label(), note)
	}
	for _, c := range o.Active {
		line := fmt.Sprintf("• %s %s", c.Name, c.Label())
		if c.NextAction.Label != "" {
			line += "｜次: " + c.NextAction.Label
			if !c.NextAction.Due.IsZero() && c.NextAction.Due.After(now) {
				line += "（" + dispatch.Clock(c.NextAction.Due, h.Loc) + "まで）"
			}
		}
		b.WriteString(line + "\n")
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func (h *Handler) deadlines(ctx context.Context) (string, error) {
	now := h.now()
	msgs, err := h.Store.ListReceivedSince(ctx, now.AddDate(0, 0, -60))
	if err != nil {
		return "", err
	}
	horizon := now.AddDate(0, 0, 7)
	var due []*model.Message
	for _, m := range msgs {
		if dl, ok := dispatch.DeadlineOf(m); ok && m.Triage != nil && m.Triage.Tier != model.TierRecord && dl.After(now) && !dl.After(horizon) {
			due = append(due, m)
		}
	}
	if len(due) == 0 {
		return "⏰ 7日以内の締切はありません。", nil
	}
	sort.Slice(due, func(i, j int) bool {
		a, _ := dispatch.DeadlineOf(due[i])
		b, _ := dispatch.DeadlineOf(due[j])
		return a.Before(b)
	})
	var b strings.Builder
	b.WriteString("⏰ 7日以内の締切\n")
	for _, m := range due {
		dl, _ := dispatch.DeadlineOf(m)
		fmt.Fprintf(&b, "• %s（%s）— %s\n", dispatch.Clock(dl, h.Loc), dispatch.Remaining(dl.Sub(now)), dispatch.Title(m))
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func (h *Handler) today(ctx context.Context) (string, error) {
	now := h.now()
	var b strings.Builder
	y, mo, d := now.Date()
	start := time.Date(y, mo, d, 0, 0, 0, 0, h.Loc)
	fmt.Fprintf(&b, "📅 今日の予定（%02d/%02d）\n", mo, d)
	if h.Calendar == nil {
		b.WriteString("（カレンダーが未設定です）\n")
	} else if events, err := h.Calendar.Events(ctx, start, start.AddDate(0, 0, 1)); err != nil {
		h.Log.Warn("予定を取得できません", "err", err)
		b.WriteString("（予定を取得できませんでした）\n")
	} else if len(events) == 0 {
		b.WriteString("予定はありません\n")
	} else {
		for _, e := range events {
			when := "終日"
			if !e.AllDay {
				when = e.Start.In(h.Loc).Format("15:04") + "〜" + e.End.In(h.Loc).Format("15:04")
			}
			fmt.Fprintf(&b, "• %s %s\n", when, e.Summary)
		}
	}

	items, err := h.Store.ListByNotify(ctx, model.NotifyDigest, 50)
	if err != nil {
		return "", err
	}
	if len(items) == 0 {
		b.WriteString("\n📬 次のまとめ報告を待っているメールはありません")
	} else {
		fmt.Fprintf(&b, "\n📬 次のまとめ報告を待っているメール（%d通）\n", len(items))
		for i, m := range items {
			if i == 10 {
				fmt.Fprintf(&b, "…ほか %d 通\n", len(items)-10)
				break
			}
			fmt.Fprintf(&b, "• %s\n", dispatch.Title(m))
		}
	}
	if until, err := h.Store.PausedUntil(ctx); err == nil && until.After(now) {
		fmt.Fprintf(&b, "\n⏸ 通知は %s まで一時停止中です", dispatch.Clock(until, h.Loc))
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// maxPause は /pause で止められる最長の時間。止めたまま忘れるのを防ぐ。
const maxPause = 7 * 24 * time.Hour

func (h *Handler) pause(ctx context.Context, hours float64) (string, error) {
	d := time.Duration(hours * float64(time.Hour))
	if d <= 0 {
		return "停止する時間は正の数で指定してください。", nil
	}
	if d > maxPause {
		d = maxPause
	}
	until := h.now().Add(d)
	if err := h.Store.SetPausedUntil(ctx, until); err != nil {
		return "", err
	}
	return fmt.Sprintf("⏸ %s まで通知を止めます。/resume で再開できます。止めている間のメールは、再開後のまとめ報告に載ります。",
		dispatch.Clock(until, h.Loc)), nil
}

func (h *Handler) resume(ctx context.Context) (string, error) {
	if err := h.Store.SetPausedUntil(ctx, time.Time{}); err != nil {
		return "", err
	}
	return "▶ 通知を再開しました。止めていた間のメールは、次のまとめ報告に載ります。", nil
}

// button は選考状況の「確定」「取り消す」ボタンを処理する。custom_id は "sel:<confirm|undo>:<メールID>"。
func (h *Handler) button(ctx context.Context, in discord.Interaction) discord.Response {
	parts := strings.SplitN(in.Data.CustomID, ":", 3)
	if len(parts) == 3 && parts[0] == "prop" && h.Proposals != nil {
		return h.proposalButton(ctx, parts[1], parts[2])
	}
	if len(parts) != 3 || parts[0] != "sel" {
		return discord.Message("このボタンは使えません。", true)
	}
	op, msgID := parts[1], parts[2]
	m, err := h.Store.GetMessage(ctx, msgID)
	if err != nil || m == nil || m.SelectionCompanyID == "" {
		h.Log.Error("ボタンのメールが見つかりません", "messageId", msgID, "err", err)
		return discord.Message("対象のメールが見つかりません（保存期間を過ぎた可能性があります）。", true)
	}

	var c *model.Company
	var ok bool
	var done string
	switch op {
	case "confirm":
		c, ok, err = h.Tracker.Confirm(ctx, m.SelectionCompanyID, msgID)
		done = "✅ 確定しました"
	case "undo":
		c, ok, err = h.Tracker.Undo(ctx, m.SelectionCompanyID, msgID)
		done = "↩️ 取り消しました"
	default:
		return discord.Message("このボタンは使えません。", true)
	}
	switch {
	case errors.Is(err, selection.ErrNotLatest):
		return discord.Message("この後に別の更新があるため、取り消せません。/status で今の状況を確認してください。", true)
	case err != nil:
		h.Log.Error("ボタンの処理に失敗", "op", op, "err", err)
		return discord.Message("処理に失敗しました。時間をおいてもう一度試してください。", true)
	case !ok:
		return discord.Message("この操作は処理済みです。", true)
	}
	original := ""
	if in.Message != nil {
		original = in.Message.Content
	}
	return discord.Update(fmt.Sprintf("%s\n\n%s（%s は今「%s」）", original, done, c.Name, c.Label()))
}
