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
	"github.com/Tatsubooom/sheepbutler/internal/source"
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
		{"メールの転送", checkForwarding},
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
	raw, err := requireEnv("MAIL_SOURCES")
	if err != nil {
		return "", err
	}
	srcs, err := source.Parse(raw)
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
	// 本番と同じ判別処理で、直近のメールがどの転送元から来たかを数える。
	res, err := svc.Users.Messages.List("me").Q("in:inbox newer_than:14d").MaxResults(100).Context(ctx).Do()
	if err != nil {
		return "", err
	}
	count := map[string]int{}
	for _, m := range res.Messages {
		full, err := svc.Users.Messages.Get("me", m.Id).Format("metadata").MetadataHeaders(source.Headers()...).Context(ctx).Do()
		if err != nil {
			return "", err
		}
		headers := map[string][]string{}
		for _, h := range full.Payload.Headers {
			headers[h.Name] = append(headers[h.Name], h.Value)
		}
		count[source.Detect(srcs, headers)]++
	}
	var ok, missing []string
	for _, s := range srcs {
		if n := count[s.Label]; n > 0 {
			ok = append(ok, fmt.Sprintf("%s %d 通", s.Label, n))
		} else {
			missing = append(missing, s.Label+"（"+s.Address+"）")
		}
	}
	if n := count[source.Unknown]; n > 0 {
		ok = append(ok, fmt.Sprintf("転送元不明 %d 通", n))
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("過去14日間に次の宛先のメールが見つかりません: %s。転送設定を確認し、そのアドレスにテストメールを送ってください（届いたもの: %s）",
			strings.Join(missing, "、"), strings.Join(ok, "、"))
	}
	return "転送を確認: " + strings.Join(ok, "、"), nil
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

func notifierFromEnv() (notify.Notifier, error) { return notify.FromEnv() }
