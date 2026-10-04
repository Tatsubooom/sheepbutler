package notify

import (
	"context"
	"net/http"
)

// Discord は Bot トークンで指定チャンネルにメッセージを送る。
type Discord struct {
	BotToken  string
	ChannelID string
}

func (d *Discord) Name() string { return "Discord" }

func (d *Discord) Send(ctx context.Context, text string) error {
	h := http.Header{}
	h.Set("Authorization", "Bot "+d.BotToken)
	// メンションによる一斉通知を防ぐため、allowed_mentions を空にする。
	body := map[string]any{
		"content":          text,
		"allowed_mentions": map[string]any{"parse": []string{}},
	}
	return postJSON(ctx, "https://discord.com/api/v10/channels/"+d.ChannelID+"/messages", h, body)
}
