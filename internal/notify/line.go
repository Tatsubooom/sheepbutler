package notify

import (
	"context"
	"net/http"
)

// LINE は Messaging API のプッシュメッセージで送る。
// プッシュは無料プランの月間送信数に数えられるため、多用しないこと。
type LINE struct {
	ChannelAccessToken string
	UserID             string
}

func (l *LINE) Name() string { return "LINE" }

// LINE の1吹き出しの文字数の上限と、1回のプッシュに含められる吹き出しの数。
// 1回のプッシュは吹き出しの数に関係なく送信数 1 と数えられる。
const (
	lineMax         = 5000
	lineMaxMessages = 5
)

// Send は text を送る。長い場合は吹き出しを分け、5つを超える分は切り捨てる。
func (l *LINE) Send(ctx context.Context, text string) error {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+l.ChannelAccessToken)
	var msgs []map[string]string
	for _, chunk := range Split(text, lineMax) {
		if len(msgs) == lineMaxMessages {
			break
		}
		msgs = append(msgs, map[string]string{"type": "text", "text": chunk})
	}
	body := map[string]any{"to": l.UserID, "messages": msgs}
	return postJSON(ctx, "https://api.line.me/v2/bot/message/push", h, body)
}
