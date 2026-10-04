// Package store は Firestore への保存を扱う。
package store

import (
	"context"
	"time"

	"cloud.google.com/go/firestore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

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
