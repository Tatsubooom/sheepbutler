package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const apiBase = "https://discord.com/api/v10"

// Client は Bot トークンで Discord の REST API を呼ぶ（セットアップ用）。
type Client struct {
	BotToken string
	HTTP     *http.Client
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, apiBase+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bot "+c.BotToken)
	req.Header.Set("Content-Type", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, b)
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

// AppInfo は Bot のアプリケーションの情報。
type AppInfo struct {
	ID        string `json:"id"`
	VerifyKey string `json:"verify_key"` // 署名の検証に使う公開鍵
	Owner     User   `json:"owner"`      // アプリの持ち主（操作を許す人）
}

func (c *Client) AppInfo(ctx context.Context) (AppInfo, error) {
	var a AppInfo
	return a, c.do(ctx, http.MethodGet, "/applications/@me", nil, &a)
}

// GuildOfChannel はチャンネルが属するサーバーの ID を返す。
func (c *Client) GuildOfChannel(ctx context.Context, channelID string) (string, error) {
	var ch struct {
		GuildID string `json:"guild_id"`
	}
	return ch.GuildID, c.do(ctx, http.MethodGet, "/channels/"+channelID, nil, &ch)
}

// Command はスラッシュコマンドの定義。
type Command struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Options     []CommandOption `json:"options,omitempty"`
}

type CommandOption struct {
	Type        int    `json:"type"` // 3 文字列 / 10 数値
	Name        string `json:"name"`
	Description string `json:"description"`
	Required    bool   `json:"required"`
}

// RegisterGuildCommands はサーバー専用のコマンドを登録し直す（すぐに使えるようになる）。
func (c *Client) RegisterGuildCommands(ctx context.Context, appID, guildID string, cmds []Command) error {
	return c.do(ctx, http.MethodPut, "/applications/"+appID+"/guilds/"+guildID+"/commands", cmds, nil)
}

// SetInteractionsEndpoint はボタンやコマンドの送り先 URL を設定する。
// Discord はこのとき URL に署名付きの確認（PING）を送り、正しく応答できなければ拒否する。
func (c *Client) SetInteractionsEndpoint(ctx context.Context, url string) error {
	return c.do(ctx, http.MethodPatch, "/applications/@me", map[string]string{"interactions_endpoint_url": url}, nil)
}

// EditOriginal は「考え中…」で応答した後に、結果で書き換える（Bot トークンは不要）。
func EditOriginal(ctx context.Context, appID, token string, data ResponseData) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, apiBase+"/webhooks/"+appID+"/"+token+"/messages/@original", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, msg)
	}
	return nil
}

// Commands は SheepButler のスラッシュコマンド。
var Commands = []Command{
	{Name: "status", Description: "選考中の企業と、次にやることを表示する"},
	{Name: "deadlines", Description: "7日以内の締切を表示する"},
	{Name: "today", Description: "今日の予定と、確認してほしいメールを表示する"},
	{Name: "pause", Description: "通知を一時停止する（緊急停止）", Options: []CommandOption{
		{Type: 10, Name: "hours", Description: "停止する時間（時間単位、既定 24）"},
	}},
	{Name: "resume", Description: "一時停止した通知を再開する"},
	{Name: "ask", Description: "執事に自由に質問・依頼する（例: 今どこまで進んでる？ / B社は辞退した）", Options: []CommandOption{
		{Type: 3, Name: "question", Description: "質問や依頼", Required: true},
	}},
}
