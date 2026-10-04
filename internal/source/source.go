// Package source は、専用 Gmail に転送されてきたメールが元々どのアカウント宛てだったかを判別する。
//
// 転送元のアカウントは環境変数 MAIL_SOURCES に「ラベル=アドレス」を ; 区切りで並べて指定する。
//
//	MAIL_SOURCES=大学=12abc345@ms.dendai.ac.jp;個人=someone@gmail.com
package source

import (
	"fmt"
	"regexp"
	"strings"
)

// Unknown はどの転送元にも一致しなかったときのラベル。
const Unknown = "不明"

type Source struct {
	Label   string
	Address string
}

// Parse は MAIL_SOURCES の値を解釈する。
func Parse(s string) ([]Source, error) {
	var out []Source
	seen := map[string]bool{}
	for _, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		label, addr, ok := strings.Cut(part, "=")
		label, addr = strings.TrimSpace(label), strings.ToLower(strings.TrimSpace(addr))
		if !ok || label == "" || !strings.Contains(addr, "@") {
			return nil, fmt.Errorf("MAIL_SOURCES の %q は「ラベル=アドレス」の形式ではありません", part)
		}
		if seen[normalize(addr)] {
			return nil, fmt.Errorf("MAIL_SOURCES にアドレス %s が重複しています", addr)
		}
		seen[normalize(addr)] = true
		out = append(out, Source{Label: label, Address: addr})
	}
	return out, nil
}

// detectHeaders は判別に使うヘッダー。上にあるものほど確実なので先に調べる。
//   - X-Forwarded-To / X-Forwarded-For / X-Original-To: 転送時に付くことがある元の宛先
//   - Received: 「for <address>」に最初の受信者が残る
//   - To / Cc: 転送後も元の宛先が残る（BCC やメーリングリストでは含まれないことがある）
var detectHeaders = []string{"X-Forwarded-To", "X-Forwarded-For", "X-Original-To", "Received", "To", "Cc"}

// Headers は Gmail API に要求すべきヘッダー名の一覧を返す。
func Headers() []string { return append([]string(nil), detectHeaders...) }

var addrPattern = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)

// Detect はヘッダーから転送元のラベルを返す。headers は名前→値（同名ヘッダーは複数）。
// 一致しなければ Unknown を返す。
func Detect(sources []Source, headers map[string][]string) string {
	byAddr := make(map[string]string, len(sources))
	for _, s := range sources {
		byAddr[normalize(s.Address)] = s.Label
	}
	for _, name := range detectHeaders {
		for _, v := range headers[name] {
			for _, a := range addrPattern.FindAllString(v, -1) {
				if label, ok := byAddr[normalize(a)]; ok {
					return label
				}
			}
		}
	}
	return Unknown
}

// normalize は同じ受信箱に届くアドレスを同一視するための正規形を返す。
// Gmail はローカル部のドットと「+」以降を無視し、googlemail.com も同じアカウントになる。
// それ以外のドメインは大文字小文字だけを無視する。
func normalize(addr string) string {
	local, domain, ok := strings.Cut(strings.ToLower(addr), "@")
	if !ok {
		return strings.ToLower(addr)
	}
	if domain == "googlemail.com" {
		domain = "gmail.com"
	}
	if domain == "gmail.com" {
		local, _, _ = strings.Cut(local, "+")
		local = strings.ReplaceAll(local, ".", "")
	}
	return local + "@" + domain
}
