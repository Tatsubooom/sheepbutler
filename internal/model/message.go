// Package model は Firestore に保存するデータの型を定義する。
package model

import (
	"time"

	"github.com/Tatsubooom/sheepbutler/internal/llm"
)

// MessageStatus はメールの処理段階。
type MessageStatus string

const (
	// StatusNew は取り込み済みで、必要度判定（M2）を待っている状態。
	StatusNew MessageStatus = "new"
	// StatusTriaged は必要度判定が済んだ状態。
	StatusTriaged MessageStatus = "triaged"
)

// Tier は通知の段階。
type Tier string

const (
	TierUrgent Tier = "urgent" // 即時に知らせる
	TierDigest Tier = "digest" // まとめ報告に含める
	TierRecord Tier = "record" // 記録のみ
)

// NotifyState は通知の状態。
type NotifyState string

const (
	NotifyNone   NotifyState = "none"   // 知らせない（記録のみ）
	NotifyUrgent NotifyState = "urgent" // 即時通知を待っている
	NotifyDigest NotifyState = "digest" // 次のまとめ報告を待っている
	NotifyDone   NotifyState = "done"   // 知らせ済み
)

// NotifyStateFor は判定の段階から最初の通知の状態を決める。
func NotifyStateFor(t Tier) NotifyState {
	switch t {
	case TierUrgent:
		return NotifyUrgent
	case TierDigest:
		return NotifyDigest
	}
	return NotifyNone
}

// Notification は notifications/{id} に保存する送信の記録。回数の上限の判定と監査に使う。
type Notification struct {
	At         time.Time `firestore:"at"`
	Kind       string    `firestore:"kind"` // urgent / digest / reminder / guard
	Channel    string    `firestore:"channel"`
	MessageIDs []string  `firestore:"messageIds"`
	ExpiresAt  time.Time `firestore:"expiresAt"`
}

// TriageMethod は判定に何を使ったか。
type TriageMethod string

const (
	MethodLLM      TriageMethod = "llm"      // LLM の抽出結果とルール
	MethodRules    TriageMethod = "rules"    // ルールのみ（予算超過や LLM の失敗時）
	MethodExcluded TriageMethod = "excluded" // 除外リストに当たった
)

// TriageResult は必要度判定の結果。
type TriageResult struct {
	Tier       Tier         `firestore:"tier"`
	Score      float64      `firestore:"score"`
	Deadline   float64      `firestore:"deadline"`   // 期限の点数
	Importance float64      `firestore:"importance"` // 重要度の点数
	Interest   float64      `firestore:"interest"`   // 興味度の点数
	Reasons    []string     `firestore:"reasons"`    // 通知に添える理由
	Method     TriageMethod `firestore:"method"`
	At         time.Time    `firestore:"at"`
}

// Audit は audit/{id} に保存する LLM 呼び出しの記録。
type Audit struct {
	At           time.Time `firestore:"at"`
	MessageID    string    `firestore:"messageId"`
	Model        string    `firestore:"model"`
	InputChars   int       `firestore:"inputChars"`
	InputTokens  int64     `firestore:"inputTokens"`
	OutputTokens int64     `firestore:"outputTokens"`
	CostUSD      float64   `firestore:"costUsd"`
	Tier         Tier      `firestore:"tier"`
	Error        string    `firestore:"error"`
	ExpiresAt    time.Time `firestore:"expiresAt"`
}

// Message は messages/{gmailId} に保存するメール1通分の記録。
// 本文は保存しない。判定に必要な部分は M2 で都度 Gmail から取得する。
type Message struct {
	ID         string    `firestore:"-"`
	ThreadID   string    `firestore:"threadId"`
	ReceivedAt time.Time `firestore:"receivedAt"`
	// Source は転送元アカウントのラベル（MAIL_SOURCES で定義。判別できなければ「不明」）。
	Source     string        `firestore:"source"`
	From       string        `firestore:"from"`
	To         string        `firestore:"to"`
	Subject    string        `firestore:"subject"`
	Snippet    string        `firestore:"snippet"`
	LabelIDs   []string      `firestore:"labelIds"`
	Status     MessageStatus `firestore:"status"`
	IngestedAt time.Time     `firestore:"ingestedAt"`
	// ExpiresAt を過ぎると Firestore の TTL ポリシーで自動削除される。
	ExpiresAt time.Time `firestore:"expiresAt"`

	// Extracted は LLM が抜き出した判定材料。Triage は判定結果。どちらも判定前は nil。
	Extracted      *llm.Extraction `firestore:"extracted,omitempty"`
	Triage         *TriageResult   `firestore:"triage,omitempty"`
	TriageAttempts int             `firestore:"triageAttempts"`

	// Notify は通知の状態。判定時に段階から決まり、送ったら NotifyDone になる。
	Notify NotifyState `firestore:"notify"`
	// NotifiedAt は即時通知かまとめ報告で知らせた時刻。
	NotifiedAt time.Time `firestore:"notifiedAt"`
	// Reminded は送り済みの締切リマインド（締切の何時間前か）。
	Reminded []int `firestore:"reminded"`

	// Headers は転送元の判別に使う生のヘッダー。保存はしない。
	Headers map[string][]string `firestore:"-"`
}

// GmailState は state/gmail に保存する同期の状態。
type GmailState struct {
	// HistoryID はここまで処理済みという Gmail の履歴 ID。0 は未初期化。
	HistoryID      uint64
	WatchExpiresAt time.Time
}
