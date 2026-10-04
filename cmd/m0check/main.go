// m0check は M0（準備）が完了したかを確認する。外部サービスへの接続を1つずつ試し、結果を表示する。
//
//	go run ./cmd/m0check            # 全項目
//	go run ./cmd/m0check -skip-notify  # 通知のテスト送信を省く
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"

	"github.com/Tatsubooom/sheepbutler/internal/envfile"
	"github.com/Tatsubooom/sheepbutler/internal/googleauth"
	"github.com/Tatsubooom/sheepbutler/internal/notify"
)

type check struct {
	name string
	run  func(ctx context.Context) (string, error)
}

func main() {
	skipNotify := flag.Bool("skip-notify", false, "Discord/LINE へのテスト送信を省く")
	flag.Parse()

	if err := envfile.Load(".env"); err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	checks := []check{
		{"Gmail（専用アカウント）", checkGmail},
		{"大学メールの転送", checkForwarding},
		{"Google カレンダー", checkCalendar},
		{"Claude API", checkClaude},
	}
	if !*skipNotify {
		checks = append(checks, check{"通知のテスト送信", checkNotify})
	}

	failed := 0
	for _, c := range checks {
		detail, err := c.run(ctx)
		if err != nil {
			failed++
			fmt.Printf("✗ %s\n    %v\n", c.name, err)
			continue
		}
		fmt.Printf("✓ %s\n    %s\n", c.name, detail)
	}
	fmt.Println()
	if failed > 0 {
		fmt.Printf("%d 件失敗しました。docs/setup/m0.md の該当手順を確認してください。\n", failed)
		os.Exit(1)
	}
	fmt.Println("M0 の準備はすべて完了しています。")
}

func requireEnv(key string) (string, error) {
	v := os.Getenv(key)
	if v == "" {
		return "", fmt.Errorf("環境変数 %s が未設定です（.env を確認）", key)
	}
	return v, nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func googleOption(ctx context.Context) (option.ClientOption, error) {
	hc, err := googleauth.HTTPClient(ctx,
		getenv("GOOGLE_OAUTH_CLIENT_FILE", ".secrets/client_secret.json"),
		getenv("GOOGLE_TOKEN_FILE", ".secrets/token.json"))
	if err != nil {
		return nil, err
	}
	return option.WithHTTPClient(hc), nil
}

func checkGmail(ctx context.Context) (string, error) {
	opt, err := googleOption(ctx)
	if err != nil {
		return "", err
	}
	svc, err := gmail.NewService(ctx, opt)
	if err != nil {
		return "", err
	}
	p, err := svc.Users.GetProfile("me").Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("プロフィールを取得できません: %w", err)
	}
	return fmt.Sprintf("%s として接続（メール %d 通）", p.EmailAddress, p.MessagesTotal), nil
}

func checkForwarding(ctx context.Context) (string, error) {
	addr, err := requireEnv("UNIVERSITY_MAIL_ADDRESS")
	if err != nil {
		return "", err
	}
	opt, err := googleOption(ctx)
	if err != nil {
		return "", err
	}
	svc, err := gmail.NewService(ctx, opt)
	if err != nil {
		return "", err
	}
	// 転送されたメールは元の宛先（大学アドレス）を To に残すため、それで検索する。
	res, err := svc.Users.Messages.List("me").Q("to:" + addr + " newer_than:14d").MaxResults(1).Context(ctx).Do()
	if err != nil {
		return "", err
	}
	if len(res.Messages) == 0 {
		return "", errors.New("過去14日間に大学アドレス宛てのメールが見つかりません。転送ルールを確認し、大学アドレスにテストメールを送ってください")
	}
	m, err := svc.Users.Messages.Get("me", res.Messages[0].Id).Format("metadata").MetadataHeaders("Subject").Context(ctx).Do()
	if err != nil {
		return "", err
	}
	subject := ""
	for _, h := range m.Payload.Headers {
		if h.Name == "Subject" {
			subject = h.Value
		}
	}
	return fmt.Sprintf("転送を確認（最新: %q）", truncate(subject, 40)), nil
}

func checkCalendar(ctx context.Context) (string, error) {
	calID, err := requireEnv("GOOGLE_CALENDAR_ID")
	if err != nil {
		return "", err
	}
	opt, err := googleOption(ctx)
	if err != nil {
		return "", err
	}
	svc, err := calendar.NewService(ctx, opt)
	if err != nil {
		return "", err
	}
	now := time.Now()
	ev, err := svc.Events.List(calID).
		TimeMin(now.Format(time.RFC3339)).
		TimeMax(now.Add(7 * 24 * time.Hour).Format(time.RFC3339)).
		SingleEvents(true).MaxResults(50).Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("カレンダー %s を読めません。メインアカウントのカレンダーを専用 Gmail に共有したか確認してください: %w", calID, err)
	}
	return fmt.Sprintf("%s を読み取り可能（今後7日の予定 %d 件）", calID, len(ev.Items)), nil
}

func checkClaude(ctx context.Context) (string, error) {
	if _, err := requireEnv("ANTHROPIC_API_KEY"); err != nil {
		return "", err
	}
	client := anthropic.NewClient() // ANTHROPIC_API_KEY を自動で読む
	model := getenv("LLM_MODEL", "claude-haiku-4-5")
	resp, err := client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(model),
		MaxTokens: 16,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock("「OK」とだけ返してください。")),
		},
	})
	if err != nil {
		var apierr *anthropic.Error
		if errors.As(err, &apierr) {
			switch apierr.StatusCode {
			case 401:
				return "", errors.New("API キーが無効です")
			case 404:
				return "", fmt.Errorf("モデル %s が見つかりません", model)
			}
		}
		return "", err
	}
	return fmt.Sprintf("%s が応答（入力 %d / 出力 %d トークン）", model, resp.Usage.InputTokens, resp.Usage.OutputTokens), nil
}

func checkNotify(ctx context.Context) (string, error) {
	n, err := notifierFromEnv()
	if err != nil {
		return "", err
	}
	if err := n.Send(ctx, "SheepButler の接続テストです。これが届いていれば通知の準備は完了です。"); err != nil {
		return "", fmt.Errorf("%s への送信に失敗: %w", n.Name(), err)
	}
	return n.Name() + " に送信しました。届いているか確認してください", nil
}

func notifierFromEnv() (notify.Notifier, error) {
	switch ch := strings.ToLower(getenv("NOTIFY_CHANNEL", "discord")); ch {
	case "discord":
		tok, err := requireEnv("DISCORD_BOT_TOKEN")
		if err != nil {
			return nil, err
		}
		cid, err := requireEnv("DISCORD_CHANNEL_ID")
		if err != nil {
			return nil, err
		}
		return &notify.Discord{BotToken: tok, ChannelID: cid}, nil
	case "line":
		tok, err := requireEnv("LINE_CHANNEL_ACCESS_TOKEN")
		if err != nil {
			return nil, err
		}
		uid, err := requireEnv("LINE_USER_ID")
		if err != nil {
			return nil, err
		}
		return &notify.LINE{ChannelAccessToken: tok, UserID: uid}, nil
	default:
		return nil, fmt.Errorf("NOTIFY_CHANNEL は discord か line を指定してください（現在: %q）", ch)
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
