// Package ingest は Gmail の新着を差分で取得し、Firestore に取り込む。
package ingest

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Tatsubooom/sheepbutler/internal/mail"
	"github.com/Tatsubooom/sheepbutler/internal/model"
)

// Mailbox は取り込みに必要な Gmail の操作。
type Mailbox interface {
	Profile(ctx context.Context) (uint64, error)
	History(ctx context.Context, startID uint64) ([]string, uint64, error)
	Recent(ctx context.Context, query string, max int64) ([]string, error)
	Get(ctx context.Context, id string) (*model.Message, error)
	Watch(ctx context.Context, topic string) (uint64, time.Time, error)
}

// Store は取り込みに必要な保存先の操作。
type Store interface {
	GmailState(ctx context.Context) (model.GmailState, error)
	AdvanceHistoryID(ctx context.Context, id uint64) error
	SetWatchExpiration(ctx context.Context, exp time.Time) error
	MessageExists(ctx context.Context, id string) (bool, error)
	CreateMessage(ctx context.Context, m *model.Message) (bool, error)
}

// resyncQuery は履歴が期限切れのときに取り直す範囲。
const (
	resyncQuery = "in:inbox newer_than:7d"
	resyncMax   = 200
)

type Syncer struct {
	Mailbox   Mailbox
	Store     Store
	Retention time.Duration
	Now       func() time.Time
}

// Result は1回の同期の結果。ログに出す。
type Result struct {
	Initialized bool // 初回のため履歴 ID の記録だけ行った
	Resynced    bool // 履歴が期限切れのため直近のメールを取り直した
	Found       int
	Created     int
	Skipped     int
}

func (s *Syncer) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Sync は前回の履歴 ID 以降に受信したメールを取り込む。
// 途中で失敗した場合は履歴 ID を進めずにエラーを返すので、Pub/Sub の再送で続きから処理される。
// 取り込み済みのメールは保存時に弾かれるため、何度呼んでも同じ結果になる。
func (s *Syncer) Sync(ctx context.Context) (Result, error) {
	var res Result
	st, err := s.Store.GmailState(ctx)
	if err != nil {
		return res, fmt.Errorf("状態の読み込み: %w", err)
	}

	// 初回は過去のメールを遡らず、今の時点を起点にする。
	if st.HistoryID == 0 {
		hid, err := s.Mailbox.Profile(ctx)
		if err != nil {
			return res, fmt.Errorf("プロフィールの取得: %w", err)
		}
		res.Initialized = true
		return res, s.Store.AdvanceHistoryID(ctx, hid)
	}

	ids, latest, err := s.Mailbox.History(ctx, st.HistoryID)
	if errors.Is(err, mail.ErrHistoryExpired) {
		res.Resynced = true
		if latest, err = s.Mailbox.Profile(ctx); err != nil {
			return res, fmt.Errorf("プロフィールの取得: %w", err)
		}
		if ids, err = s.Mailbox.Recent(ctx, resyncQuery, resyncMax); err != nil {
			return res, fmt.Errorf("直近のメールの取得: %w", err)
		}
	} else if err != nil {
		return res, fmt.Errorf("履歴の取得: %w", err)
	}
	res.Found = len(ids)

	for _, id := range ids {
		created, err := s.ingestOne(ctx, id)
		if err != nil {
			return res, fmt.Errorf("メール %s の取り込み: %w", id, err)
		}
		if created {
			res.Created++
		} else {
			res.Skipped++
		}
	}
	return res, s.Store.AdvanceHistoryID(ctx, latest)
}

func (s *Syncer) ingestOne(ctx context.Context, id string) (bool, error) {
	exists, err := s.Store.MessageExists(ctx, id)
	if err != nil || exists {
		return false, err
	}
	m, err := s.Mailbox.Get(ctx, id)
	if err != nil {
		return false, err
	}
	now := s.now()
	m.Status = model.StatusNew
	m.IngestedAt = now
	m.ExpiresAt = m.ReceivedAt.Add(s.Retention)
	return s.Store.CreateMessage(ctx, m)
}

// RenewWatch は Gmail の watch を張り直す。watch は約7日で切れるため毎日呼ぶ。
// 初回であれば、watch が返した履歴 ID を同期の起点として記録する。
func (s *Syncer) RenewWatch(ctx context.Context, topic string) (time.Time, error) {
	hid, exp, err := s.Mailbox.Watch(ctx, topic)
	if err != nil {
		return time.Time{}, fmt.Errorf("watch の登録: %w", err)
	}
	if err := s.Store.SetWatchExpiration(ctx, exp); err != nil {
		return time.Time{}, err
	}
	st, err := s.Store.GmailState(ctx)
	if err != nil {
		return time.Time{}, err
	}
	if st.HistoryID == 0 {
		if err := s.Store.AdvanceHistoryID(ctx, hid); err != nil {
			return time.Time{}, err
		}
	}
	return exp, nil
}
