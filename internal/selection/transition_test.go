package selection

import (
	"testing"
	"time"

	"github.com/Tatsubooom/sheepbutler/internal/llm"
	"github.com/Tatsubooom/sheepbutler/internal/model"
)

func TestNext(t *testing.T) {
	due := time.Date(2026, 10, 10, 23, 59, 0, 0, time.UTC)
	tests := []struct {
		name      string
		cur       model.Stage
		round     int
		ev        llm.SelectionEvent
		wantKind  Kind
		wantStage model.Stage
		wantRound int
	}{
		{"エントリー", model.StageInterested, 0, llm.EventEntryConfirmed, KindAuto, model.StageEntry, 0},
		{"書類の依頼", model.StageEntry, 0, llm.EventDocumentRequest, KindAuto, model.StageDocuments, 0},
		{"面接の日程調整で1次面接へ", model.StageDocuments, 0, llm.EventInterviewScheduling, KindAuto, model.StageInterview, 1},
		{"いきなり面接の案内（エントリーの記録なし）", model.StageInterested, 0, llm.EventInterviewScheduling, KindAuto, model.StageInterview, 1},
		{"書類通過で1次面接へ", model.StageDocuments, 0, llm.EventResultPass, KindAuto, model.StageInterview, 1},
		{"1次面接通過で2次へ", model.StageInterview, 1, llm.EventResultPass, KindAuto, model.StageInterview, 2},
		{"2次の日程調整は次数を変えない", model.StageInterview, 2, llm.EventInterviewScheduling, KindIgnore, model.StageInterview, 2},
		{"前の段階のメールは無視", model.StageInterview, 1, llm.EventEntryConfirmed, KindIgnore, model.StageInterview, 1},
		{"内定は確認待ち", model.StageInterview, 3, llm.EventOffer, KindTentative, model.StageOffer, 3},
		{"不合格は確認待ち", model.StageInterview, 1, llm.EventResultFail, KindTentative, model.StageRejected, 1},
		{"不合格の重複は無視", model.StageRejected, 1, llm.EventResultFail, KindIgnore, model.StageRejected, 1},
		{"不合格の後の面接案内は要確認", model.StageRejected, 1, llm.EventInterviewScheduling, KindReview, model.StageRejected, 1},
		{"内定の後の選考案内は要確認", model.StageOffer, 3, llm.EventDocumentRequest, KindReview, model.StageOffer, 3},
		{"辞退した企業の連絡は要確認", model.StageDeclined, 0, llm.EventResultPass, KindReview, model.StageDeclined, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Next(tt.cur, tt.round, tt.ev, due)
			if got.Kind != tt.wantKind || got.Stage != tt.wantStage || got.Round != tt.wantRound {
				t.Errorf("got %s %s %d, want %s %s %d", got.Kind, got.Stage, got.Round, tt.wantKind, tt.wantStage, tt.wantRound)
			}
		})
	}
}

func TestNextKeepsNewDeadline(t *testing.T) {
	due := time.Date(2026, 10, 10, 23, 59, 0, 0, time.UTC)
	// 2次面接の日程調整の再送（締切が変わった）: 段階は変えないが、次にやることの期限は更新する
	got := Next(model.StageInterview, 2, llm.EventInterviewScheduling, due)
	if got.Kind != KindIgnore || got.NextAction.Label != "面接日程の回答" || !got.NextAction.Due.Equal(due) {
		t.Fatalf("%+v", got)
	}
}
