// Package googleauth は Gmail・Google カレンダーを読むための OAuth 設定とトークンを扱う。
// 権限は読み取り専用の2つだけに限定する。
package googleauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/gmail/v1"
)

// Scopes は SheepButler が要求する権限の全て。書き込み権限は含めない。
var Scopes = []string{
	gmail.GmailReadonlyScope,
	calendar.CalendarReadonlyScope,
}

// LoadConfig は Google Cloud からダウンロードした OAuth クライアント JSON（デスクトップアプリ）を読み込む。
func LoadConfig(clientFile string) (*oauth2.Config, error) {
	b, err := os.ReadFile(clientFile)
	if err != nil {
		return nil, fmt.Errorf("OAuth クライアントファイルを読めません: %w", err)
	}
	return parseConfig(b)
}

func parseConfig(b []byte) (*oauth2.Config, error) {
	cfg, err := google.ConfigFromJSON(b, Scopes...)
	if err != nil {
		return nil, fmt.Errorf("OAuth クライアントファイルの形式が不正です: %w", err)
	}
	return cfg, nil
}

// LoadToken は保存済みのトークンを読み込む。
func LoadToken(tokenFile string) (*oauth2.Token, error) {
	b, err := os.ReadFile(tokenFile)
	if err != nil {
		return nil, fmt.Errorf("トークンを読めません（先に go run ./cmd/authorize を実行してください）: %w", err)
	}
	return parseToken(b)
}

func parseToken(b []byte) (*oauth2.Token, error) {
	var tok oauth2.Token
	if err := json.Unmarshal(b, &tok); err != nil {
		return nil, fmt.Errorf("トークンの形式が不正です: %w", err)
	}
	return &tok, nil
}

// SaveToken はトークンを所有者のみ読み書きできる権限で保存する。
func SaveToken(tokenFile string, tok *oauth2.Token) error {
	if err := os.MkdirAll(filepath.Dir(tokenFile), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(tok, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(tokenFile, b, 0o600)
}

// HTTPClient は保存済みトークンを使い、期限切れなら自動更新する HTTP クライアントを返す。
func HTTPClient(ctx context.Context, clientFile, tokenFile string) (*http.Client, error) {
	cfg, err := LoadConfig(clientFile)
	if err != nil {
		return nil, err
	}
	tok, err := LoadToken(tokenFile)
	if err != nil {
		return nil, err
	}
	return cfg.Client(ctx, tok), nil
}

// HTTPClientFromEnv は環境変数から OAuth クライアントとトークンを読み、HTTP クライアントを返す。
// Cloud Run では Secret Manager の値が GOOGLE_OAUTH_CLIENT_JSON / GOOGLE_OAUTH_TOKEN_JSON に入る。
// それらが無ければ、ローカル用に GOOGLE_OAUTH_CLIENT_FILE / GOOGLE_TOKEN_FILE のファイルを読む。
func HTTPClientFromEnv(ctx context.Context) (*http.Client, error) {
	clientJSON, tokenJSON := os.Getenv("GOOGLE_OAUTH_CLIENT_JSON"), os.Getenv("GOOGLE_OAUTH_TOKEN_JSON")
	if clientJSON == "" || tokenJSON == "" {
		return HTTPClient(ctx, getenv("GOOGLE_OAUTH_CLIENT_FILE", ".secrets/client_secret.json"), getenv("GOOGLE_TOKEN_FILE", ".secrets/token.json"))
	}
	cfg, err := parseConfig([]byte(clientJSON))
	if err != nil {
		return nil, err
	}
	tok, err := parseToken([]byte(tokenJSON))
	if err != nil {
		return nil, err
	}
	return cfg.Client(ctx, tok), nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
