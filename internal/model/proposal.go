package model

import "time"

// ProposalKind は変更案の種類。
type ProposalKind string

const (
	ProposalStatus   ProposalKind = "status"   // 選考状況の変更（例: 辞退にする）
	ProposalInterest ProposalKind = "interest" // 志望度の変更
	ProposalSetting  ProposalKind = "setting"  // 設定の変更
)

// ProposalState は変更案の状態。
type ProposalState string

const (
	ProposalPending   ProposalState = "pending"
	ProposalApplied   ProposalState = "applied"
	ProposalCancelled ProposalState = "cancelled"
)

// Proposal は proposals/{id} に保存する、チャットが作った変更案。
// チャットの LLM は変更案を作るだけで、反映は利用者がボタンを押したときにだけ行う。
type Proposal struct {
	ID       string        `firestore:"-"`
	Kind     ProposalKind  `firestore:"kind"`
	State    ProposalState `firestore:"state"`
	Summary  string        `firestore:"summary"` // 利用者に見せる説明（例: "A社 を 辞退 にする"）
	Company  string        `firestore:"company"`
	Stage    Stage         `firestore:"stage"`
	Round    int           `firestore:"round"`
	Interest string        `firestore:"interest"`
	Key      string        `firestore:"key"`
	Value    any           `firestore:"value"`
	Reason   string        `firestore:"reason"`
	// ExpiresAt を過ぎた変更案は反映できず、TTL で自動削除される。
	CreatedAt time.Time `firestore:"createdAt"`
	ExpiresAt time.Time `firestore:"expiresAt"`
}
