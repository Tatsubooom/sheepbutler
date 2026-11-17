// Package store は Firestore への保存を扱う。
package store

import (
	"context"
	"fmt"
	"sort"
	"time"

	"cloud.google.com/go/firestore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/Tatsubooom/sheepbutler/internal/llm"
	"github.com/Tatsubooom/sheepbutler/internal/model"
)

const (
	colMessages = "messages"
	colState    = "state"
	docGmail    = "gmail"
)

// gmailStateDoc は state/gmail の保存形式。Firestore は uint64 を扱えないため int64 で持つ。
type gmailStateDoc struct {
	HistoryID      int64     `firestore:"historyId"`
	WatchExpiresAt time.Time `firestore:"watchExpiresAt"`
	UpdatedAt      time.Time `firestore:"updatedAt"`
}

type Firestore struct {
	c *firestore.Client
}

func NewFirestore(ctx context.Context, projectID string) (*Firestore, error) {
	c, err := firestore.NewClient(ctx, projectID)
	if err != nil {
		return nil, err
	}
	return &Firestore{c: c}, nil
}

func (f *Firestore) Close() error { return f.c.Close() }

func (f *Firestore) gmailDoc() *firestore.DocumentRef {
	return f.c.Collection(colState).Doc(docGmail)
}

// GmailState は同期の状態を返す。未作成ならゼロ値を返す。
func (f *Firestore) GmailState(ctx context.Context) (model.GmailState, error) {
	var d gmailStateDoc
	snap, err := f.gmailDoc().Get(ctx)
	if status.Code(err) == codes.NotFound {
		return model.GmailState{}, nil
	}
	if err != nil {
		return model.GmailState{}, err
	}
	if err := snap.DataTo(&d); err != nil {
		return model.GmailState{}, err
	}
	return model.GmailState{HistoryID: uint64(d.HistoryID), WatchExpiresAt: d.WatchExpiresAt}, nil
}

// AdvanceHistoryID は保存済みの履歴 ID より大きい場合だけ更新する。
// Pub/Sub の通知が並行して処理されても、履歴 ID が巻き戻らないようにトランザクションで行う。
func (f *Firestore) AdvanceHistoryID(ctx context.Context, id uint64) error {
	ref := f.gmailDoc()
	return f.c.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		var cur gmailStateDoc
		snap, err := tx.Get(ref)
		switch {
		case status.Code(err) == codes.NotFound:
		case err != nil:
			return err
		default:
			if err := snap.DataTo(&cur); err != nil {
				return err
			}
		}
		if int64(id) <= cur.HistoryID {
			return nil
		}
		return tx.Set(ref, map[string]any{
			"historyId": int64(id),
			"updatedAt": time.Now(),
		}, firestore.MergeAll)
	})
}

// SetWatchExpiration は watch の期限を記録する。
func (f *Firestore) SetWatchExpiration(ctx context.Context, exp time.Time) error {
	_, err := f.gmailDoc().Set(ctx, map[string]any{
		"watchExpiresAt": exp,
		"updatedAt":      time.Now(),
	}, firestore.MergeAll)
	return err
}

// MessageExists はメールが取り込み済みかを返す。Gmail API の呼び出しを省くために使う。
func (f *Firestore) MessageExists(ctx context.Context, id string) (bool, error) {
	_, err := f.c.Collection(colMessages).Doc(id).Get(ctx)
	if status.Code(err) == codes.NotFound {
		return false, nil
	}
	return err == nil, err
}

// CreateMessage はメールを保存する。既に存在する場合は何もせず false を返す（二重処理の防止）。
func (f *Firestore) CreateMessage(ctx context.Context, m *model.Message) (bool, error) {
	_, err := f.c.Collection(colMessages).Doc(m.ID).Create(ctx, m)
	if status.Code(err) == codes.AlreadyExists {
		return false, nil
	}
	return err == nil, err
}

const (
	colUsage = "usage"
	colAudit = "audit"
)

// ListPending は未判定（status=new）のメールを最大 limit 件返す。
func (f *Firestore) ListPending(ctx context.Context, limit int) ([]*model.Message, error) {
	docs, err := f.c.Collection(colMessages).Where("status", "==", string(model.StatusNew)).Limit(limit).Documents(ctx).GetAll()
	if err != nil {
		return nil, err
	}
	out := make([]*model.Message, 0, len(docs))
	for _, d := range docs {
		var m model.Message
		if err := d.DataTo(&m); err != nil {
			return nil, fmt.Errorf("messages/%s: %w", d.Ref.ID, err)
		}
		m.ID = d.Ref.ID
		out = append(out, &m)
	}
	return out, nil
}

// ListTriaged は判定済みのメールを受信の新しい順に最大 limit 件返す（重みの調整用）。
func (f *Firestore) ListTriaged(ctx context.Context, limit int) ([]*model.Message, error) {
	docs, err := f.c.Collection(colMessages).Where("status", "==", string(model.StatusTriaged)).Limit(limit).Documents(ctx).GetAll()
	if err != nil {
		return nil, err
	}
	out := make([]*model.Message, 0, len(docs))
	for _, d := range docs {
		var m model.Message
		if err := d.DataTo(&m); err != nil {
			return nil, fmt.Errorf("messages/%s: %w", d.Ref.ID, err)
		}
		m.ID = d.Ref.ID
		out = append(out, &m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ReceivedAt.After(out[j].ReceivedAt) })
	return out, nil
}

// SaveTriage は判定結果を保存し、状態を triaged にする。
// queue が true なら段階に応じて通知待ちにする（再計算のときは false にして、昔のメールを再通知しない）。
func (f *Firestore) SaveTriage(ctx context.Context, id string, ex *llm.Extraction, res model.TriageResult, queue bool) error {
	ups := []firestore.Update{
		{Path: "extracted", Value: ex},
		{Path: "triage", Value: res},
		{Path: "status", Value: string(model.StatusTriaged)},
	}
	if queue {
		ups = append(ups, firestore.Update{Path: "notify", Value: string(model.NotifyStateFor(res.Tier))})
	}
	_, err := f.c.Collection(colMessages).Doc(id).Update(ctx, ups)
	return err
}

// ResetToPending は判定をやり直すために状態を new に戻す（判定の調整用）。
func (f *Firestore) ResetToPending(ctx context.Context, id string) error {
	_, err := f.c.Collection(colMessages).Doc(id).Update(ctx, []firestore.Update{
		{Path: "status", Value: string(model.StatusNew)},
		{Path: "triageAttempts", Value: 0},
	})
	return err
}

// IncrementAttempts は LLM の失敗回数を1増やし、増やした後の値を返す。
func (f *Firestore) IncrementAttempts(ctx context.Context, id string) (int, error) {
	ref := f.c.Collection(colMessages).Doc(id)
	n := 0
	err := f.c.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		snap, err := tx.Get(ref)
		if err != nil {
			return err
		}
		var m model.Message
		if err := snap.DataTo(&m); err != nil {
			return err
		}
		n = m.TriageAttempts + 1
		return tx.Update(ref, []firestore.Update{{Path: "triageAttempts", Value: n}})
	})
	return n, err
}

// UsageCost はその日の LLM の費用（USD）を返す。
func (f *Firestore) UsageCost(ctx context.Context, day string) (float64, error) {
	snap, err := f.c.Collection(colUsage).Doc(day).Get(ctx)
	if status.Code(err) == codes.NotFound {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	v, err := snap.DataAt("costUsd")
	if err != nil {
		return 0, nil
	}
	switch x := v.(type) {
	case float64:
		return x, nil
	case int64:
		return float64(x), nil
	}
	return 0, nil
}

// AddUsage はその日の LLM の使用量に加算する（同時に呼ばれても正しく足される）。
func (f *Firestore) AddUsage(ctx context.Context, day string, u llm.Usage) error {
	_, err := f.c.Collection(colUsage).Doc(day).Set(ctx, map[string]any{
		"costUsd":      firestore.Increment(u.CostUSD),
		"inputTokens":  firestore.Increment(u.InputTokens),
		"outputTokens": firestore.Increment(u.OutputTokens),
		"calls":        firestore.Increment(1),
		"updatedAt":    time.Now(),
	}, firestore.MergeAll)
	return err
}

// AddAudit は LLM 呼び出しの記録を残す。expiresAt を過ぎると TTL で自動削除される。
func (f *Firestore) AddAudit(ctx context.Context, a model.Audit) error {
	_, _, err := f.c.Collection(colAudit).Add(ctx, a)
	return err
}

const (
	colNotifications = "notifications"
	docDigest        = "digest"
)

func (f *Firestore) messagesFrom(docs []*firestore.DocumentSnapshot) ([]*model.Message, error) {
	out := make([]*model.Message, 0, len(docs))
	for _, d := range docs {
		var m model.Message
		if err := d.DataTo(&m); err != nil {
			return nil, fmt.Errorf("messages/%s: %w", d.Ref.ID, err)
		}
		m.ID = d.Ref.ID
		out = append(out, &m)
	}
	return out, nil
}

// ListByNotify は通知の状態が state のメールを最大 limit 件返す。
func (f *Firestore) ListByNotify(ctx context.Context, state model.NotifyState, limit int) ([]*model.Message, error) {
	docs, err := f.c.Collection(colMessages).Where("notify", "==", string(state)).Limit(limit).Documents(ctx).GetAll()
	if err != nil {
		return nil, err
	}
	return f.messagesFrom(docs)
}

// ListReceivedSince は since 以降に受信したメールを返す。
// 状態などとの組み合わせは複合インデックスが要るため、受信日時だけで絞ってコード側で選ぶ。
func (f *Firestore) ListReceivedSince(ctx context.Context, since time.Time) ([]*model.Message, error) {
	docs, err := f.c.Collection(colMessages).Where("receivedAt", ">=", since).Limit(1000).Documents(ctx).GetAll()
	if err != nil {
		return nil, err
	}
	return f.messagesFrom(docs)
}

// SetNotify はメールの通知の状態を変える。at が0でなければ通知した時刻も記録する。
func (f *Firestore) SetNotify(ctx context.Context, ids []string, state model.NotifyState, at time.Time) error {
	for _, id := range ids {
		ups := []firestore.Update{{Path: "notify", Value: string(state)}}
		if !at.IsZero() {
			ups = append(ups, firestore.Update{Path: "notifiedAt", Value: at})
		}
		if _, err := f.c.Collection(colMessages).Doc(id).Update(ctx, ups); err != nil {
			return fmt.Errorf("messages/%s: %w", id, err)
		}
	}
	return nil
}

// AddReminded は送り済みのリマインド（締切の何時間前か）を追加する。
func (f *Firestore) AddReminded(ctx context.Context, id string, hours []int) error {
	vals := make([]any, len(hours))
	for i, h := range hours {
		vals[i] = h
	}
	_, err := f.c.Collection(colMessages).Doc(id).Update(ctx, []firestore.Update{
		{Path: "reminded", Value: firestore.ArrayUnion(vals...)},
	})
	return err
}

// ListNotificationsSince は since 以降の送信記録を返す（回数の上限の判定に使う）。
func (f *Firestore) ListNotificationsSince(ctx context.Context, since time.Time) ([]model.Notification, error) {
	docs, err := f.c.Collection(colNotifications).Where("at", ">=", since).Documents(ctx).GetAll()
	if err != nil {
		return nil, err
	}
	out := make([]model.Notification, 0, len(docs))
	for _, d := range docs {
		var n model.Notification
		if err := d.DataTo(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

// AddNotification は送信を記録する。expiresAt を過ぎると TTL で自動削除される。
func (f *Firestore) AddNotification(ctx context.Context, n model.Notification) error {
	_, _, err := f.c.Collection(colNotifications).Add(ctx, n)
	return err
}

// LastDigest は最後に送ったまとめ報告の予定時刻を返す。
func (f *Firestore) LastDigest(ctx context.Context) (time.Time, error) {
	snap, err := f.c.Collection(colState).Doc(docDigest).Get(ctx)
	if status.Code(err) == codes.NotFound {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	v, err := snap.DataAt("lastSlot")
	if err != nil {
		return time.Time{}, nil
	}
	t, _ := v.(time.Time)
	return t, nil
}

// SetLastDigest は送ったまとめ報告の予定時刻を記録する。
func (f *Firestore) SetLastDigest(ctx context.Context, slot time.Time) error {
	_, err := f.c.Collection(colState).Doc(docDigest).Set(ctx, map[string]any{"lastSlot": slot, "updatedAt": time.Now()})
	return err
}

const (
	colCompanies = "companies"
	colHistory   = "history"
)

// GetCompany は企業を返す。無ければ nil。
func (f *Firestore) GetCompany(ctx context.Context, id string) (*model.Company, error) {
	snap, err := f.c.Collection(colCompanies).Doc(id).Get(ctx)
	if status.Code(err) == codes.NotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c model.Company
	if err := snap.DataTo(&c); err != nil {
		return nil, err
	}
	c.ID = id
	return &c, nil
}

// SaveCompany は企業と変化の記録をトランザクションで保存する。同じメールの記録が既にあれば false を返す。
func (f *Firestore) SaveCompany(ctx context.Context, c *model.Company, h model.History) (bool, error) {
	ref := f.c.Collection(colCompanies).Doc(c.ID)
	href := ref.Collection(colHistory).Doc(h.MessageID)
	applied := false
	err := f.c.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		applied = false
		if _, err := tx.Get(href); err == nil {
			return nil // このメールは処理済み
		} else if status.Code(err) != codes.NotFound {
			return err
		}
		if err := tx.Set(ref, c); err != nil {
			return err
		}
		applied = true
		return tx.Create(href, h)
	})
	return applied, err
}

// ListCompanies はすべての企業を返す。
func (f *Firestore) ListCompanies(ctx context.Context) ([]*model.Company, error) {
	docs, err := f.c.Collection(colCompanies).Documents(ctx).GetAll()
	if err != nil {
		return nil, err
	}
	out := make([]*model.Company, 0, len(docs))
	for _, d := range docs {
		var c model.Company
		if err := d.DataTo(&c); err != nil {
			return nil, fmt.Errorf("companies/%s: %w", d.Ref.ID, err)
		}
		c.ID = d.Ref.ID
		out = append(out, &c)
	}
	return out, nil
}

// SetSelectionChange はメールに選考状況の変化を書き添える。
func (f *Firestore) SetSelectionChange(ctx context.Context, messageID, text, companyID, kind string) error {
	_, err := f.c.Collection(colMessages).Doc(messageID).Update(ctx, []firestore.Update{
		{Path: "selectionChange", Value: text},
		{Path: "selectionCompanyId", Value: companyID},
		{Path: "selectionKind", Value: kind},
	})
	return err
}

// GetHistory は変化の記録を返す。無ければ nil。
func (f *Firestore) GetHistory(ctx context.Context, companyID, historyID string) (*model.History, error) {
	snap, err := f.c.Collection(colCompanies).Doc(companyID).Collection(colHistory).Doc(historyID).Get(ctx)
	if status.Code(err) == codes.NotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var h model.History
	return &h, snap.DataTo(&h)
}

// GetMessage はメールを返す。無ければ nil。
func (f *Firestore) GetMessage(ctx context.Context, id string) (*model.Message, error) {
	snap, err := f.c.Collection(colMessages).Doc(id).Get(ctx)
	if status.Code(err) == codes.NotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m model.Message
	if err := snap.DataTo(&m); err != nil {
		return nil, err
	}
	m.ID = id
	return &m, nil
}

const docPause = "pause"

// PausedUntil は通知を止めている期限を返す。止めていなければゼロ値。
func (f *Firestore) PausedUntil(ctx context.Context) (time.Time, error) {
	snap, err := f.c.Collection(colState).Doc(docPause).Get(ctx)
	if status.Code(err) == codes.NotFound {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	v, err := snap.DataAt("until")
	if err != nil {
		return time.Time{}, nil
	}
	t, _ := v.(time.Time)
	return t, nil
}

// SetPausedUntil は通知を止める期限を設定する。ゼロ値なら再開する。
func (f *Firestore) SetPausedUntil(ctx context.Context, until time.Time) error {
	_, err := f.c.Collection(colState).Doc(docPause).Set(ctx, map[string]any{"until": until, "updatedAt": time.Now()})
	return err
}

const (
	colProposals = "proposals"
	docSettings  = "settings"
)

// CreateProposal は変更案を保存し、ID を返す。
func (f *Firestore) CreateProposal(ctx context.Context, p *model.Proposal) (string, error) {
	ref, _, err := f.c.Collection(colProposals).Add(ctx, p)
	if err != nil {
		return "", err
	}
	return ref.ID, nil
}

// GetProposal は変更案を返す。無ければ nil。
func (f *Firestore) GetProposal(ctx context.Context, id string) (*model.Proposal, error) {
	snap, err := f.c.Collection(colProposals).Doc(id).Get(ctx)
	if status.Code(err) == codes.NotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p model.Proposal
	if err := snap.DataTo(&p); err != nil {
		return nil, err
	}
	p.ID = id
	return &p, nil
}

// TransitionProposal は変更案の状態を from から to に変える。既に from でなければ false を返す（二重押しの防止）。
func (f *Firestore) TransitionProposal(ctx context.Context, id string, from, to model.ProposalState) (bool, error) {
	ref := f.c.Collection(colProposals).Doc(id)
	changed := false
	err := f.c.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		changed = false
		snap, err := tx.Get(ref)
		if err != nil {
			return err
		}
		var p model.Proposal
		if err := snap.DataTo(&p); err != nil {
			return err
		}
		if p.State != from {
			return nil
		}
		changed = true
		return tx.Update(ref, []firestore.Update{{Path: "state", Value: string(to)}})
	})
	return changed, err
}

// SettingOverrides はチャットで変更した設定（"項目名" → 値）を返す。
func (f *Firestore) SettingOverrides(ctx context.Context) (map[string]any, error) {
	snap, err := f.c.Collection(colState).Doc(docSettings).Get(ctx)
	if status.Code(err) == codes.NotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	v, err := snap.DataAt("overrides")
	if err != nil {
		return nil, nil
	}
	m, _ := v.(map[string]any)
	return m, nil
}

// SetSettingOverride は設定の上書きを1つ保存する。
func (f *Firestore) SetSettingOverride(ctx context.Context, key string, value any) error {
	_, err := f.c.Collection(colState).Doc(docSettings).Set(ctx, map[string]any{
		"overrides": map[string]any{key: value},
		"updatedAt": time.Now(),
	}, firestore.MergeAll)
	return err
}

// SetCompanyInterest は企業の志望度を保存する。企業が無ければ「興味あり」で作る。
func (f *Firestore) SetCompanyInterest(ctx context.Context, id, name, interest string) error {
	ref := f.c.Collection(colCompanies).Doc(id)
	return f.c.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		_, err := tx.Get(ref)
		if status.Code(err) == codes.NotFound {
			return tx.Set(ref, &model.Company{Name: name, Stage: model.StageInterested, Interest: interest, CreatedAt: time.Now()})
		}
		if err != nil {
			return err
		}
		return tx.Update(ref, []firestore.Update{{Path: "interest", Value: interest}})
	})
}
