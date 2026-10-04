// authorize は専用 Gmail アカウントで OAuth の同意を行い、リフレッシュトークンを保存する。
// ローカル PC で1回だけ実行する（ブラウザが開く）。
//
//	go run ./cmd/authorize
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"time"

	"golang.org/x/oauth2"

	"github.com/Tatsubooom/sheepbutler/internal/envfile"
	"github.com/Tatsubooom/sheepbutler/internal/googleauth"
)

func main() {
	if err := envfile.Load(".env"); err != nil {
		log.Fatal(err)
	}
	clientFile := getenv("GOOGLE_OAUTH_CLIENT_FILE", ".secrets/client_secret.json")
	tokenFile := getenv("GOOGLE_TOKEN_FILE", ".secrets/token.json")

	cfg, err := googleauth.LoadConfig(clientFile)
	if err != nil {
		log.Fatal(err)
	}

	// デスクトップアプリ用クライアントはループバックの任意ポートへのリダイレクトを許可している。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	cfg.RedirectURL = fmt.Sprintf("http://%s/callback", ln.Addr())

	state := randomHex(16)
	verifier := oauth2.GenerateVerifier()
	// prompt=consent を付けないと、2回目以降はリフレッシュトークンが返らないことがある。
	authURL := cfg.AuthCodeURL(state,
		oauth2.AccessTypeOffline,
		oauth2.SetAuthURLParam("prompt", "consent"),
		oauth2.S256ChallengeOption(verifier),
	)

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		switch {
		case q.Get("state") != state:
			errCh <- errors.New("state が一致しません（別のリクエストの可能性があります）")
		case q.Get("error") != "":
			errCh <- fmt.Errorf("同意が拒否されました: %s", q.Get("error"))
		default:
			codeCh <- q.Get("code")
			fmt.Fprintln(w, "認可が完了しました。このタブは閉じて構いません。")
			return
		}
		http.Error(w, "認可に失敗しました。ターミナルを確認してください。", http.StatusBadRequest)
	})}
	go srv.Serve(ln)
	defer srv.Close()

	fmt.Println("ブラウザで同意画面を開きます。**専用 Gmail アカウント**でログインしてください。")
	fmt.Println("開かない場合は次の URL を手動で開いてください:\n" + authURL)
	openBrowser(authURL)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var code string
	select {
	case code = <-codeCh:
	case err := <-errCh:
		log.Fatal(err)
	case <-ctx.Done():
		log.Fatal("5分以内に同意が完了しませんでした")
	}

	tok, err := cfg.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		log.Fatalf("トークンの取得に失敗しました: %v", err)
	}
	if tok.RefreshToken == "" {
		log.Fatal("リフレッシュトークンが返されませんでした。Google アカウントの「サードパーティ製アプリ」から接続を削除して再実行してください")
	}
	if err := googleauth.SaveToken(tokenFile, tok); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("トークンを %s に保存しました。次は go run ./cmd/m0check で接続を確認してください。\n", tokenFile)
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		log.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
