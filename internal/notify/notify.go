// Package notify は Discord と LINE への送信を共通のインターフェースで扱う。
// M0 ではテキスト送信だけを持ち、ボタン付き送信は M3 で追加する。
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Notifier は通知先チャネルの共通インターフェース。
type Notifier interface {
	Name() string
	Send(ctx context.Context, text string) error
}

var httpClient = &http.Client{Timeout: 15 * time.Second}

// postJSON は JSON を送り、2xx 以外ならレスポンス本文を含むエラーを返す。
func postJSON(ctx context.Context, url string, header http.Header, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header = header.Clone()
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
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
