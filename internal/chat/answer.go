package chat

import (
	"fmt"
	"strings"

	"github.com/Tatsubooom/sheepbutler/internal/discord"
	"github.com/Tatsubooom/sheepbutler/internal/model"
)

// maxProposalRows は1つの回答に付ける変更案の上限（Discord のボタンの行は最大5つ）。
const maxProposalRows = 5

// AnswerData は /ask の回答を「考え中…」と差し替える内容にする。
// 変更案ごとに「反映する」「やめる」のボタンを付ける（押すまで何も変わらない）。
func AnswerData(text string, proposals []*model.Proposal) discord.ResponseData {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(text))
	var rows []discord.ActionRow
	for i, p := range proposals {
		if i == maxProposalRows {
			fmt.Fprintf(&b, "\n（ほか %d 件の変更案は省きました。もう一度頼んでください）", len(proposals)-maxProposalRows)
			break
		}
		if i == 0 {
			b.WriteString("\n\n📝 変更案（ボタンを押すまで反映されません）")
		}
		n := i + 1
		fmt.Fprintf(&b, "\n%d. %s", n, p.Summary)
		rows = append(rows, discord.Row(
			discord.Button{Style: discord.StyleSuccess, Label: fmt.Sprintf("%d を反映する", n), CustomID: "prop:apply:" + p.ID},
			discord.Button{Style: discord.StyleSecondary, Label: "やめる", CustomID: "prop:cancel:" + p.ID},
		))
	}
	content := b.String()
	if r := []rune(content); len(r) > 2000 {
		content = string(r[:1999]) + "…"
	}
	return discord.ResponseData{Content: content, Components: rows, AllowedMentions: map[string]any{"parse": []string{}}}
}
