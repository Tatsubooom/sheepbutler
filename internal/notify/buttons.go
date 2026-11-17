package notify

import "context"

// Button は通知に付けるボタン。押されると Discord の Interactions で CustomID が届く。
type Button struct {
	Label    string
	CustomID string
	Style    int // 1 青 / 2 灰 / 3 緑 / 4 赤
}

// ButtonSender はボタン付きで送れる通知先（Discord のみ）。
type ButtonSender interface {
	SendButtons(ctx context.Context, text string, buttons []Button) error
}

// SendButtons はボタン付きで送る。ボタンは最後の塊にだけ付ける。
func (d *Discord) SendButtons(ctx context.Context, text string, buttons []Button) error {
	chunks := Split(text, discordMax)
	for i, chunk := range chunks {
		body := map[string]any{
			"content":          chunk,
			"allowed_mentions": map[string]any{"parse": []string{}},
		}
		if i == len(chunks)-1 && len(buttons) > 0 {
			comps := make([]map[string]any, 0, len(buttons))
			for _, b := range buttons {
				comps = append(comps, map[string]any{"type": 2, "style": b.Style, "label": b.Label, "custom_id": b.CustomID})
			}
			body["components"] = []map[string]any{{"type": 1, "components": comps}}
		}
		if err := postJSON(ctx, "https://discord.com/api/v10/channels/"+d.ChannelID+"/messages", d.header(), body); err != nil {
			return err
		}
	}
	return nil
}
