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

// discordMax は Discord の1メッセージの文字数の上限。
const discordMax = 2000

// Send は text を送る。上限を超える場合は複数のメッセージに分けて送る。
func (d *Discord) Send(ctx context.Context, text string) error {
	h := d.header()
	for _, chunk := range Split(text, discordMax) {
		// メール由来の文字列に @everyone などが含まれても通知が飛ばないよう、allowed_mentions を空にする。
		body := map[string]any{
			"content":          chunk,
			"allowed_mentions": map[string]any{"parse": []string{}},
		}
		if err := postJSON(ctx, "https://discord.com/api/v10/channels/"+d.ChannelID+"/messages", h, body); err != nil {
			return err
		}
	}
	return nil
}

func (d *Discord) header() http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bot "+d.BotToken)
	return h
}
