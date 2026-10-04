package mail

import (
	"encoding/base64"
	"testing"

	"golang.org/x/text/encoding/japanese"
	"google.golang.org/api/gmail/v1"
)

func part(mimeType, charset string, data []byte) *gmail.MessagePart {
	return &gmail.MessagePart{
		MimeType: mimeType,
		Headers:  []*gmail.MessagePartHeader{{Name: "Content-Type", Value: mimeType + "; charset=" + charset}},
		Body:     &gmail.MessagePartBody{Data: base64.URLEncoding.EncodeToString(data)},
	}
}

func TestExtractTextDecodesJapaneseCharsets(t *testing.T) {
	iso, _ := japanese.ISO2022JP.NewEncoder().Bytes([]byte("面接日程のご連絡です。"))
	sjis, _ := japanese.ShiftJIS.NewEncoder().Bytes([]byte("締切は明日です。"))

	if got := extractText(part("text/plain", "ISO-2022-JP", iso)); got != "面接日程のご連絡です。" {
		t.Errorf("ISO-2022-JP: %q", got)
	}
	if got := extractText(part("text/plain", "Shift_JIS", sjis)); got != "締切は明日です。" {
		t.Errorf("Shift_JIS: %q", got)
	}
}

func TestExtractTextPrefersPlainOverHTML(t *testing.T) {
	root := &gmail.MessagePart{MimeType: "multipart/alternative", Parts: []*gmail.MessagePart{
		part("text/html", "utf-8", []byte("<p>HTML版</p>")),
		part("text/plain", "utf-8", []byte("テキスト版")),
	}}
	if got := extractText(root); got != "テキスト版" {
		t.Errorf("got %q", got)
	}
}

func TestExtractTextFromHTML(t *testing.T) {
	html := `<html><style>p{color:red}</style><body><p>【重要】&amp;締切</p><div>10月10日&nbsp;まで</div><script>alert(1)</script></body></html>`
	got := extractText(part("text/html", "utf-8", []byte(html)))
	if got != "【重要】&締切\n10月10日 まで" {
		t.Errorf("got %q", got)
	}
}

func TestExtractTextSkipsAttachments(t *testing.T) {
	att := part("text/plain", "utf-8", []byte("添付ファイルの中身"))
	att.Filename = "memo.txt"
	root := &gmail.MessagePart{MimeType: "multipart/mixed", Parts: []*gmail.MessagePart{att, part("text/plain", "utf-8", []byte("本文"))}}
	if got := extractText(root); got != "本文" {
		t.Errorf("got %q", got)
	}
}

func TestTruncateCountsRunes(t *testing.T) {
	if got := truncate("あいうえお", 3); got != "あいう…" {
		t.Errorf("got %q", got)
	}
}
