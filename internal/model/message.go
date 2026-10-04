// Package model は Firestore に保存するデータの型を定義する。
package model

import "time"

// MessageStatus はメールの処理段階。
type MessageStatus string

const (
	// StatusNew は取り込み済みで、必要度判定（M2）を待っている状態。
	StatusNew MessageStatus = "new"
)

// Message は messages/{gmailId} に保存するメール1通分の記録。
// 本文は保存しない。判定に必要な部分は M2 で都度 Gmail から取得する。
type Message struct {
	ID         string        `firestore:"-"`
	ThreadID   string        `firestore:"threadId"`
	ReceivedAt time.Time     `firestore:"receivedAt"`
	From       string        `firestore:"from"`
	To         string        `firestore:"to"`
	Subject    string        `firestore:"subject"`
	Snippet    string        `firestore:"snippet"`
	LabelIDs   []string      `firestore:"labelIds"`
	Status     MessageStatus `firestore:"status"`
	IngestedAt time.Time     `firestore:"ingestedAt"`
	// ExpiresAt を過ぎると Firestore の TTL ポリシーで自動削除される。
	ExpiresAt time.Time `firestore:"expiresAt"`
}

// GmailState は state/gmail に保存する同期の状態。
type GmailState struct {
	// HistoryID はここまで処理済みという Gmail の履歴 ID。0 は未初期化。
	HistoryID      uint64
	WatchExpiresAt time.Time
}
