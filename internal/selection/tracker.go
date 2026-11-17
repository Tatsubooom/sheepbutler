package selection

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/Tatsubooom/sheepbutler/internal/config"
	"github.com/Tatsubooom/sheepbutler/internal/llm"
	"github.com/Tatsubooom/sheepbutler/internal/model"
)

// Store は選考状況の保存先。
type Store interface {
	// GetCompany は企業を返す。無ければ nil。
	GetCompany(ctx context.Context, id string) (*model.Company, error)
	// SaveCompany は企業と変化の記録を1つのトランザクションで保存する。
	// 同じメールの記録が既にあれば何もせず false を返す（二重更新の防止）。
	SaveCompany(ctx context.Context, c *model.Company, h model.History) (bool, error)
	ListCompanies(ctx context.Context) ([]*model.Company, error)
	// GetHistory は変化の記録を返す。無ければ nil。
	GetHistory(ctx context.Context, companyID, historyID string) (*model.History, error)
	// SetSelectionChange はメールに選考状況の変化を書き添える（即時通知に表示し、ボタンを付ける）。
	SetSelectionChange(ctx context.Context, messageID, text, companyID, kind string) error
}

type Tracker struct {
	Store  Store
	Config config.Config
	Log    *slog.Logger
	Now    func() time.Time
}

func (t *Tracker) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

// CompanyID は企業名から文書 ID を作る。表記揺れは同じ ID になる。
func CompanyID(name string) string {
	id := strings.ReplaceAll(config.NormalizeCompany(name), "/", "_")
	if id == "" || id == "." || id == ".." {
		return "_unknown"
	}
	return id
}

// Apply は判定済みのメールから選考状況を更新する。選考に関係ないメールでは何もしない。
func (t *Tracker) Apply(ctx context.Context, m *model.Message, ex *llm.Extraction) error {
	if ex == nil || ex.SelectionEvent == llm.EventNone || strings.TrimSpace(ex.Company) == "" || ex.MailKind == llm.KindBulk {
		return nil
	}
	name, autoCreated := ex.Company, true
	if co, ok := t.Config.FindCompany(ex.Company); ok {
		if co.Interest == "none" {
			return nil // 興味なしと登録した企業は追わない
		}
		name, autoCreated = co.Name, false
	}

	now := t.now()
	id := CompanyID(name)
	c, err := t.Store.GetCompany(ctx, id)
	if err != nil {
		return err
	}
	if c == nil {
		c = &model.Company{ID: id, Name: name, Stage: model.StageInterested, AutoCreated: autoCreated, CreatedAt: now}
	}

	dl, _ := ex.DeadlineTime()
	r := Next(c.Stage, c.Round, ex.SelectionEvent, dl)
	h := model.History{At: now, MessageID: m.ID, Event: string(ex.SelectionEvent), From: c.Label(), By: string(r.Kind), Before: c.Snapshot()}
	if m.ReceivedAt.After(c.LastContactAt) {
		c.LastContactAt = m.ReceivedAt
	}

	change := ""
	switch r.Kind {
	case KindAuto, KindTentative:
		before := c.Label()
		c.Stage, c.Round, c.NextAction = r.Stage, r.Round, r.NextAction
		c.Unconfirmed = r.Kind == KindTentative
		change = before + " → " + c.Label()
		if r.Kind == KindTentative {
			change += "（要確認）"
		}
	case KindReview:
		c.NeedsReview, c.ReviewNote = true, r.Note
		change = "要確認: " + r.Note
	case KindIgnore:
		if r.NextAction.Label != "" {
			c.NextAction = r.NextAction // 同じ段階の再送で期限が変わった
		}
	}
	if change != "" {
		c.LastChange, c.ChangedAt, c.LastHistoryID = change, now, m.ID
	}
	h.To = c.Label()

	applied, err := t.Store.SaveCompany(ctx, c, h)
	if err != nil || !applied || change == "" {
		return err
	}
	if t.Log != nil {
		t.Log.Info("選考状況を更新しました", "company", c.ID, "kind", r.Kind, "to", c.Stage, "round", c.Round)
	}
	return t.Store.SetSelectionChange(ctx, m.ID, name+" "+change, c.ID, string(r.Kind))
}

// ErrNotLatest は、取り消そうとした変化の後に別の変化があったことを示す。
var ErrNotLatest = errors.New("この後に別の更新があるため取り消せません")

// Confirm は確認待ち（内定？・不合格？）や要確認を確定する。historyID は変化を起こしたメールの ID。
// 処理済みなら false を返す。
func (t *Tracker) Confirm(ctx context.Context, companyID, historyID string) (*model.Company, bool, error) {
	c, err := t.Store.GetCompany(ctx, companyID)
	if err != nil || c == nil {
		return nil, false, notFound(err, companyID)
	}
	before := c.Snapshot()
	c.Unconfirmed, c.NeedsReview, c.ReviewNote = false, false, ""
	c.LastChange, c.ChangedAt = "確定: "+c.Label(), t.now()
	h := model.History{At: t.now(), MessageID: historyID + "-confirm", From: before.Label(), To: c.Label(), By: "confirm", Before: before}
	ok, err := t.Store.SaveCompany(ctx, c, h)
	return c, ok, err
}

// Undo は historyID の変化を取り消し、変化の前の状態に戻す。最新の変化だけを取り消せる。
func (t *Tracker) Undo(ctx context.Context, companyID, historyID string) (*model.Company, bool, error) {
	c, err := t.Store.GetCompany(ctx, companyID)
	if err != nil || c == nil {
		return nil, false, notFound(err, companyID)
	}
	if c.LastHistoryID == historyID+"-undo" {
		return c, false, nil // 取り消し済み
	}
	if c.LastHistoryID != historyID {
		return c, false, ErrNotLatest
	}
	h0, err := t.Store.GetHistory(ctx, companyID, historyID)
	if err != nil || h0 == nil {
		return nil, false, notFound(err, companyID+"/"+historyID)
	}
	from := c.Label()
	c.Restore(h0.Before)
	c.LastChange, c.ChangedAt, c.LastHistoryID = "取り消し: "+from+" → "+c.Label(), t.now(), historyID+"-undo"
	h := model.History{At: t.now(), MessageID: historyID + "-undo", From: from, To: c.Label(), By: "undo"}
	ok, err := t.Store.SaveCompany(ctx, c, h)
	return c, ok, err
}

func notFound(err error, what string) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("%s が見つかりません", what)
}

// Overview はまとめ報告に載せる選考状況。
type Overview struct {
	Active  []*model.Company // 選考中（段階の進んだ順）
	Changed []*model.Company // since 以降に変化があった
	Review  []*model.Company // 要確認（内定・不合格の確認待ち、判断できない変化）
	Stale   []*model.Company // 連絡が途絶えている
}

// Overview は選考状況をまとめる。since は前回のまとめ報告の時刻。
func (t *Tracker) Overview(ctx context.Context, since time.Time) (Overview, error) {
	var o Overview
	cs, err := t.Store.ListCompanies(ctx)
	if err != nil {
		return o, err
	}
	now := t.now()
	staleBefore := now.Add(-time.Duration(t.Config.JobHunt.StaleAfterDays) * 24 * time.Hour)
	for _, c := range cs {
		if c.ChangedAt.After(since) {
			o.Changed = append(o.Changed, c)
		}
		if c.Unconfirmed || c.NeedsReview {
			o.Review = append(o.Review, c)
		}
		if c.Stage.Terminal() || c.Stage == model.StageInterested {
			continue
		}
		o.Active = append(o.Active, c)
		if t.Config.JobHunt.StaleAfterDays > 0 && (c.Stage == model.StageDocuments || c.Stage == model.StageInterview) &&
			c.LastContactAt.Before(staleBefore) {
			o.Stale = append(o.Stale, c)
		}
	}
	sort.SliceStable(o.Active, func(i, j int) bool {
		a, b := o.Active[i], o.Active[j]
		if model.StageOrder[a.Stage] != model.StageOrder[b.Stage] {
			return model.StageOrder[a.Stage] > model.StageOrder[b.Stage]
		}
		return a.Round > b.Round
	})
	return o, nil
}
