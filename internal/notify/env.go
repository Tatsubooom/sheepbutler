package notify

import (
	"fmt"
	"os"
	"strings"
)

// FromEnv は環境変数 NOTIFY_CHANNEL（discord / line）に応じた通知先を返す。
//
//	discord: DISCORD_BOT_TOKEN, DISCORD_CHANNEL_ID
//	line:    LINE_CHANNEL_ACCESS_TOKEN, LINE_USER_ID
func FromEnv() (Notifier, error) {
	ch := strings.ToLower(os.Getenv("NOTIFY_CHANNEL"))
	if ch == "" {
		ch = "discord"
	}
	need := func(keys ...string) ([]string, error) {
		vals := make([]string, len(keys))
		for i, k := range keys {
			if vals[i] = os.Getenv(k); vals[i] == "" {
				return nil, fmt.Errorf("環境変数 %s が未設定です（.env を確認）", k)
			}
		}
		return vals, nil
	}
	switch ch {
	case "discord":
		v, err := need("DISCORD_BOT_TOKEN", "DISCORD_CHANNEL_ID")
		if err != nil {
			return nil, err
		}
		return &Discord{BotToken: v[0], ChannelID: v[1]}, nil
	case "line":
		v, err := need("LINE_CHANNEL_ACCESS_TOKEN", "LINE_USER_ID")
		if err != nil {
			return nil, err
		}
		return &LINE{ChannelAccessToken: v[0], UserID: v[1]}, nil
	}
	return nil, fmt.Errorf("NOTIFY_CHANNEL は discord か line を指定してください（現在: %q）", ch)
}
