// Package discord は Discord の Interactions（スラッシュコマンドとボタン）を扱う。
// https://discord.com/developers/docs/interactions/receiving-and-responding
package discord

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"strconv"
	"time"
)

// Interaction の種類。
const (
	TypePing             = 1
	TypeApplicationCmd   = 2
	TypeMessageComponent = 3
)

// 応答の種類。
const (
	RespPong          = 1
	RespMessage       = 4 // メッセージで応答する
	RespDeferred      = 5 // 「考え中…」を表示し、後から結果を送る
	RespUpdateMessage = 7 // ボタンを押されたメッセージを書き換える
)

// FlagEphemeral は本人にだけ見える応答にする。
const FlagEphemeral = 1 << 6

// Interaction は Discord から届く操作。使う項目だけを定義する。
type Interaction struct {
	ID            string `json:"id"`
	ApplicationID string `json:"application_id"`
	Type          int    `json:"type"`
	Token         string `json:"token"`
	Data          struct {
		Name     string   `json:"name"`
		Options  []Option `json:"options"`
		CustomID string   `json:"custom_id"`
	} `json:"data"`
	Member *struct {
		User User `json:"user"`
	} `json:"member"`
	User    *User `json:"user"`
	Message *struct {
		Content string `json:"content"`
	} `json:"message"`
}

type User struct {
	ID string `json:"id"`
}

type Option struct {
	Name  string `json:"name"`
	Value any    `json:"value"`
}

// UserID は操作した人の ID を返す（サーバー内なら member.user、DM なら user）。
func (i *Interaction) UserID() string {
	if i.Member != nil {
		return i.Member.User.ID
	}
	if i.User != nil {
		return i.User.ID
	}
	return ""
}

// StringOption は文字列のオプションを返す。
func (i *Interaction) StringOption(name string) string {
	for _, o := range i.Data.Options {
		if o.Name == name {
			if s, ok := o.Value.(string); ok {
				return s
			}
		}
	}
	return ""
}

// NumberOption は数値のオプションを返す。
func (i *Interaction) NumberOption(name string) (float64, bool) {
	for _, o := range i.Data.Options {
		if o.Name == name {
			if f, ok := o.Value.(float64); ok {
				return f, true
			}
		}
	}
	return 0, false
}

// Response は Interaction への応答。
type Response struct {
	Type int           `json:"type"`
	Data *ResponseData `json:"data,omitempty"`
}

type ResponseData struct {
	Content         string         `json:"content"`
	Flags           int            `json:"flags,omitempty"`
	Components      []ActionRow    `json:"components"`
	AllowedMentions map[string]any `json:"allowed_mentions"`
}

// Message はメッセージで応答する。メール由来の文字列でメンションが飛ばないようにする。
func Message(content string, ephemeral bool, rows ...ActionRow) Response {
	d := &ResponseData{Content: truncate(content, 2000), Components: rows, AllowedMentions: map[string]any{"parse": []string{}}}
	if ephemeral {
		d.Flags = FlagEphemeral
	}
	if d.Components == nil {
		d.Components = []ActionRow{}
	}
	return Response{Type: RespMessage, Data: d}
}

// Update はボタンを押されたメッセージを書き換える（ボタンは消す）。
func Update(content string) Response {
	return Response{Type: RespUpdateMessage, Data: &ResponseData{
		Content: truncate(content, 2000), Components: []ActionRow{}, AllowedMentions: map[string]any{"parse": []string{}},
	}}
}

// ActionRow はボタンの行。Button は1つのボタン。
type ActionRow struct {
	Type       int      `json:"type"` // 1
	Components []Button `json:"components"`
}

type Button struct {
	Type     int    `json:"type"`  // 2
	Style    int    `json:"style"` // 1 青 / 2 灰 / 3 緑 / 4 赤
	Label    string `json:"label"`
	CustomID string `json:"custom_id"`
}

const (
	StylePrimary   = 1
	StyleSecondary = 2
	StyleSuccess   = 3
	StyleDanger    = 4
)

// Row はボタンを1行に並べる（最大5つ）。
func Row(buttons ...Button) ActionRow {
	for i := range buttons {
		buttons[i].Type = 2
	}
	return ActionRow{Type: 1, Components: buttons}
}

// maxSkew は署名のタイムスタンプとして許す時刻のずれ（リプレイ攻撃を防ぐ）。
const maxSkew = 5 * time.Minute

// Verify は Discord の署名を検証する。publicKey は16進数の Ed25519 公開鍵。
func Verify(publicKey, signature, timestamp string, body []byte, now time.Time) error {
	key, err := hex.DecodeString(publicKey)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return errors.New("公開鍵の形式が不正です")
	}
	sig, err := hex.DecodeString(signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return errors.New("署名の形式が不正です")
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return errors.New("タイムスタンプが不正です")
	}
	if d := now.Sub(time.Unix(ts, 0)); d > maxSkew || d < -maxSkew {
		return errors.New("タイムスタンプが古すぎるか新しすぎます")
	}
	if !ed25519.Verify(key, append([]byte(timestamp), body...), sig) {
		return errors.New("署名が一致しません")
	}
	return nil
}

func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}
