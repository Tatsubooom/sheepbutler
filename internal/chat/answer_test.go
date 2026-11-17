package chat

import (
	"strings"
	"testing"

	"github.com/Tatsubooom/sheepbutler/internal/model"
)

func TestAnswerDataAddsProposalButtons(t *testing.T) {
	d := AnswerData("B社を辞退にする案を作りました。", []*model.Proposal{{ID: "p1", Summary: "B社 を「辞退」にする"}})
	if !strings.Contains(d.Content, "1. B社 を「辞退」にする") || len(d.Components) != 1 {
		t.Fatalf("%+v", d)
	}
	b := d.Components[0].Components
	if b[0].CustomID != "prop:apply:p1" || b[1].CustomID != "prop:cancel:p1" {
		t.Fatalf("%+v", b)
	}
	if d.AllowedMentions == nil {
		t.Fatal("メンションを飛ばさない")
	}
}

func TestAnswerDataLimits(t *testing.T) {
	var ps []*model.Proposal
	for i := 0; i < 7; i++ {
		ps = append(ps, &model.Proposal{ID: "p", Summary: "x"})
	}
	d := AnswerData(strings.Repeat("あ", 3000), ps)
	if len(d.Components) != maxProposalRows || len([]rune(d.Content)) > 2000 {
		t.Fatalf("rows=%d len=%d", len(d.Components), len([]rune(d.Content)))
	}
}
