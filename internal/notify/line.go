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

func (l *LINE) Send(ctx context.Context, text string) error {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+l.ChannelAccessToken)
	body := map[string]any{
		"to":       l.UserID,
		"messages": []map[string]string{{"type": "text", "text": text}},
	}
	return postJSON(ctx, "https://api.line.me/v2/bot/message/push", h, body)
}
