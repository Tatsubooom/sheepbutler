package mail

import (
	"context"
	"encoding/base64"
	"html"
	"mime"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/htmlindex"
	"google.golang.org/api/gmail/v1"
)

// Body はメール本文を文字列で返す（最大 maxChars 文字）。text/plain を優先し、無ければ HTML からタグを除いて使う。
// 日本語のメールは ISO-2022-JP や Shift_JIS のことがあるため、Content-Type の charset に従って UTF-8 に直す。
func (g *Gmail) Body(ctx context.Context, id string, maxChars int) (string, error) {
	m, err := g.svc.Users.Messages.Get("me", id).Format("full").Context(ctx).Do()
	if err != nil {
		return "", err
	}
	text := extractText(m.Payload)
	if text == "" {
		text = html.UnescapeString(m.Snippet)
	}
	return truncate(text, maxChars), nil
}

// extractText はパートの木から本文を探す。text/plain を優先する。
func extractText(p *gmail.MessagePart) string {
	if p == nil {
		return ""
	}
	if s := findPart(p, "text/plain"); s != "" {
		return normalizeSpace(s)
	}
	if s := findPart(p, "text/html"); s != "" {
		return normalizeSpace(stripHTML(s))
	}
	return ""
}

func findPart(p *gmail.MessagePart, mimeType string) string {
	if strings.EqualFold(p.MimeType, mimeType) && p.Body != nil && p.Body.Data != "" && p.Filename == "" {
		return decodePart(p)
	}
	for _, c := range p.Parts {
		if s := findPart(c, mimeType); s != "" {
			return s
		}
	}
	return ""
}

// decodePart は Gmail API の base64url を解き、charset を UTF-8 に変換する。
// （Content-Transfer-Encoding は Gmail API 側で解除済み）
func decodePart(p *gmail.MessagePart) string {
	raw, err := base64.URLEncoding.DecodeString(p.Body.Data)
	if err != nil {
		if raw, err = base64.RawURLEncoding.DecodeString(p.Body.Data); err != nil {
			return ""
		}
	}
	charset := ""
	for _, h := range p.Headers {
		if strings.EqualFold(h.Name, "Content-Type") {
			if _, params, err := mime.ParseMediaType(h.Value); err == nil {
				charset = params["charset"]
			}
		}
	}
	return toUTF8(raw, charset)
}

func toUTF8(raw []byte, charset string) string {
	cs := strings.ToLower(strings.TrimSpace(charset))
	if cs == "" || cs == "utf-8" || cs == "utf8" || cs == "us-ascii" {
		return strings.ToValidUTF8(string(raw), "")
	}
	enc, err := htmlindex.Get(cs)
	if err != nil {
		return strings.ToValidUTF8(string(raw), "")
	}
	out, err := enc.NewDecoder().Bytes(raw)
	if err != nil {
		return strings.ToValidUTF8(string(raw), "")
	}
	return string(out)
}

var (
	reScriptStyle = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)
	reBreak       = regexp.MustCompile(`(?i)<(br|/p|/div|/tr|/li|/h[1-6])[^>]*>`)
	reTag         = regexp.MustCompile(`(?s)<[^>]*>`)
	reSpaces      = regexp.MustCompile(`[ \t\x{00a0}\x{3000}]+`) // 半角・タブ・&nbsp;・全角の空白
	reBlankLines  = regexp.MustCompile(`\n{3,}`)
)

func stripHTML(s string) string {
	s = reScriptStyle.ReplaceAllString(s, "")
	s = reBreak.ReplaceAllString(s, "\n")
	s = reTag.ReplaceAllString(s, "")
	return html.UnescapeString(s)
}

func normalizeSpace(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = reSpaces.ReplaceAllString(s, " ")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	return strings.TrimSpace(reBlankLines.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}

// truncate は文字数（バイトではなく）で切り詰める。
func truncate(s string, max int) string {
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max]) + "…"
}
