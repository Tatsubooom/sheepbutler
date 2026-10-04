package notify

import "strings"

// Split は text を max 文字（rune 単位）以下の塊に分ける。できるだけ行の切れ目で分ける。
func Split(text string, max int) []string {
	if max <= 0 || len([]rune(text)) <= max {
		return []string{text}
	}
	var chunks []string
	var cur []rune
	flush := func() {
		if s := strings.TrimRight(string(cur), "\n"); s != "" {
			chunks = append(chunks, s)
		}
		cur = cur[:0]
	}
	for _, line := range strings.SplitAfter(text, "\n") {
		r := []rune(line)
		if len(cur)+len(r) > max {
			flush()
		}
		// 1行だけで上限を超える場合は、その行を途中で切る。
		for len(r) > max {
			chunks = append(chunks, string(r[:max]))
			r = r[max:]
		}
		cur = append(cur, r...)
	}
	flush()
	return chunks
}
