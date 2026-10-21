// discordsetup は Discord 側の設定を Bot トークンで自動化する（scripts/deploy-chat.sh から使う）。
//
//	go run ./cmd/discordsetup info                 アプリ ID・公開鍵・持ち主・サーバー ID を KEY=VALUE で出力
//	go run ./cmd/discordsetup register -url URL     スラッシュコマンドを登録し、Interactions の送り先 URL を設定
//
// .env の DISCORD_BOT_TOKEN と DISCORD_CHANNEL_ID を使う。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/Tatsubooom/sheepbutler/internal/discord"
	"github.com/Tatsubooom/sheepbutler/internal/envfile"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("使い方: discordsetup info | register -url URL")
	}
	if err := envfile.Load(".env"); err != nil {
		log.Fatal(err)
	}
	token, channel := os.Getenv("DISCORD_BOT_TOKEN"), os.Getenv("DISCORD_CHANNEL_ID")
	if token == "" || channel == "" {
		log.Fatal("DISCORD_BOT_TOKEN と DISCORD_CHANNEL_ID を .env に設定してください")
	}
	c := &discord.Client{BotToken: token}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	app, err := c.AppInfo(ctx)
	if err != nil {
		log.Fatal(err)
	}
	guild, err := c.GuildOfChannel(ctx, channel)
	if err != nil {
		log.Fatal(err)
	}

	switch os.Args[1] {
	case "info":
		// 公開鍵・ID はどれも秘密ではない（署名の検証と、操作を許す人の判定に使う）。
		fmt.Printf("DISCORD_APPLICATION_ID=%s\nDISCORD_PUBLIC_KEY=%s\nDISCORD_OWNER_ID=%s\nDISCORD_GUILD_ID=%s\n",
			app.ID, app.VerifyKey, app.Owner.ID, guild)
	case "register":
		fs := flag.NewFlagSet("register", flag.ExitOnError)
		url := fs.String("url", "", "Interactions の送り先 URL（https://…/interactions）")
		fs.Parse(os.Args[2:])
		if *url == "" {
			log.Fatal("-url を指定してください")
		}
		if err := c.RegisterGuildCommands(ctx, app.ID, guild, discord.Commands); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("スラッシュコマンドを %d 個登録しました\n", len(discord.Commands))
		if err := c.SetInteractionsEndpoint(ctx, *url); err != nil {
			log.Fatalf("送り先 URL の設定に失敗しました（Discord の確認に応答できていない可能性）: %v", err)
		}
		fmt.Println("Interactions の送り先 URL を設定しました:", *url)
	default:
		log.Fatal("使い方: discordsetup info | register -url URL")
	}
}
