package model

import (
	"fmt"
	"time"
)

// Stage は選考の段階。
type Stage string

const (
	StageInterested Stage = "interested" // 興味あり（まだ連絡なし）
	StageEntry      Stage = "entry"      // エントリー済
	StageDocuments  Stage = "documents"  // 書類・検査
	StageInterview  Stage = "interview"  // 面接（Round 次）
	StageOffer      Stage = "offer"      // 内定
	StageRejected   Stage = "rejected"   // 不合格
	StageDeclined   Stage = "declined"   // 辞退
)

// StageOrder は段階の前後関係。終わった段階（内定・不合格・辞退）は最も後ろ。
var StageOrder = map[Stage]int{
	StageInterested: 0, StageEntry: 1, StageDocuments: 2, StageInterview: 3,
	StageOffer: 4, StageRejected: 4, StageDeclined: 4,
}

// Terminal は選考が終わった段階か。
func (s Stage) Terminal() bool { return s == StageOffer || s == StageRejected || s == StageDeclined }

// StageLabel は表示用の名前。
func StageLabel(s Stage, round int) string {
	switch s {
	case StageInterested:
		return "興味あり"
	case StageEntry:
		return "エントリー済"
	case StageDocuments:
		return "書類・検査"
	case StageInterview:
		if round > 0 {
			return fmt.Sprintf("面接（%d次）", round)
		}
		return "面接"
	case StageOffer:
		return "内定"
	case StageRejected:
		return "不合格"
	case StageDeclined:
		return "辞退"
	}
	return string(s)
}

// NextAction は次にやること。
type NextAction struct {
	Label string    `firestore:"label"`
	Due   time.Time `firestore:"due"` // 期限が無ければゼロ値
}

// Company は companies/{id} に保存する企業ごとの選考状況。id は企業名を正規化したもの。
type Company struct {
	ID         string     `firestore:"-"`
	Name       string     `firestore:"name"`
	Stage      Stage      `firestore:"stage"`
	Round      int        `firestore:"round"` // 面接の次数
	NextAction NextAction `firestore:"nextAction"`
	// Unconfirmed は内定・不合格をメールの判定だけで付けた状態（本人の確認待ち）。
	Unconfirmed bool `firestore:"unconfirmed"`
	// NeedsReview は自動では判断できない変化があった（例: 不合格の後に選考の案内が来た）。
	NeedsReview bool   `firestore:"needsReview"`
	ReviewNote  string `firestore:"reviewNote"`
	// AutoCreated は config.yaml に無い企業を、選考メールから自動で登録した。
	AutoCreated   bool      `firestore:"autoCreated"`
	LastContactAt time.Time `firestore:"lastContactAt"`
	// LastChange は直近の変化の説明（例: "書類・検査 → 面接（1次）"）。ChangedAt はその時刻。
	LastChange string    `firestore:"lastChange"`
	ChangedAt  time.Time `firestore:"changedAt"`
	CreatedAt  time.Time `firestore:"createdAt"`
}

// Label は現在の段階の表示用の名前。
func (c *Company) Label() string {
	l := StageLabel(c.Stage, c.Round)
	if c.Unconfirmed {
		l += "？"
	}
	return l
}

// History は companies/{id}/history/{messageId} に保存する変化の記録。
// メール ID を文書 ID にするので、同じメールで二重に更新されない。
type History struct {
	At        time.Time `firestore:"at"`
	MessageID string    `firestore:"messageId"`
	Event     string    `firestore:"event"`
	From      string    `firestore:"from"`
	To        string    `firestore:"to"`
	By        string    `firestore:"by"` // auto / tentative / review / manual
}
