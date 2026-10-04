// Package selection は選考メールから企業ごとの選考状況を更新する（設計書の M4）。
//
// 方針:
//   - 段階は前に進む方向だけ自動で更新する。前の段階を示すメール（リマインドの再送など）は無視する
//   - 内定・不合格はメールの判定だけでは確定させず「要確認」にする
//   - 選考が終わった企業から選考の案内が来たら「要確認」にする（判定の誤りか、別の選考の可能性）
package selection

import (
	"time"

	"github.com/Tatsubooom/sheepbutler/internal/llm"
	"github.com/Tatsubooom/sheepbutler/internal/model"
)

// Kind は更新の種類。
type Kind string

const (
	KindAuto      Kind = "auto"      // 自動で反映した
	KindTentative Kind = "tentative" // 内定・不合格を確認待ちで反映した
	KindReview    Kind = "review"    // 自動では判断できないので要確認にした
	KindIgnore    Kind = "ignore"    // 変化なし（前の段階のメールなど）
)

// Result は1通のメールによる変化。
type Result struct {
	Kind       Kind
	Stage      model.Stage
	Round      int
	NextAction model.NextAction
	Note       string // 要確認の理由
}

// Next は現在の状況と選考イベントから、次の状況を決める。deadline は対応の期限（無ければゼロ値）。
func Next(cur model.Stage, round int, ev llm.SelectionEvent, deadline time.Time) Result {
	ignore := Result{Kind: KindIgnore, Stage: cur, Round: round}

	if cur.Terminal() {
		// 同じ結果の連絡の重複は無視し、それ以外の選考メールは要確認にする。
		if ev == llm.EventOffer && cur == model.StageOffer || ev == llm.EventResultFail && cur == model.StageRejected {
			return ignore
		}
		return Result{Kind: KindReview, Stage: cur, Round: round, Note: model.StageLabel(cur, round) + "の後に選考の連絡があった"}
	}

	advance := func(to model.Stage, r int, action model.NextAction) Result {
		if model.StageOrder[to] < model.StageOrder[cur] || to == cur && r <= round {
			// 後戻り・同じ段階のメールは状況を変えないが、次にやることは新しい期限で更新する
			if action.Label != "" && to == cur && r == round {
				return Result{Kind: KindIgnore, Stage: cur, Round: round, NextAction: action}
			}
			return ignore
		}
		return Result{Kind: KindAuto, Stage: to, Round: r, NextAction: action}
	}

	switch ev {
	case llm.EventEntryConfirmed:
		return advance(model.StageEntry, 0, model.NextAction{Label: "次の選考の案内を待つ"})
	case llm.EventDocumentRequest:
		return advance(model.StageDocuments, 0, model.NextAction{Label: "書類・検査の提出", Due: deadline})
	case llm.EventInterviewScheduling:
		r := round
		if cur != model.StageInterview || r == 0 {
			r = 1
		}
		return advance(model.StageInterview, r, model.NextAction{Label: "面接日程の回答", Due: deadline})
	case llm.EventInterviewConfirmed:
		r := round
		if cur != model.StageInterview || r == 0 {
			r = 1
		}
		return advance(model.StageInterview, r, model.NextAction{Label: "面接を受ける"})
	case llm.EventResultPass:
		wait := model.NextAction{Label: "次の選考の案内を待つ"}
		switch cur {
		case model.StageInterview:
			return Result{Kind: KindAuto, Stage: model.StageInterview, Round: round + 1, NextAction: wait}
		case model.StageDocuments:
			return Result{Kind: KindAuto, Stage: model.StageInterview, Round: 1, NextAction: wait}
		default:
			return advance(model.StageDocuments, 0, wait)
		}
	case llm.EventOffer:
		return Result{Kind: KindTentative, Stage: model.StageOffer, Round: round, NextAction: model.NextAction{Label: "内定の確認・承諾の回答", Due: deadline}}
	case llm.EventResultFail:
		return Result{Kind: KindTentative, Stage: model.StageRejected, Round: round}
	}
	return ignore
}
