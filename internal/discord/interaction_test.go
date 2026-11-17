package discord

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"testing"
	"time"
)

func TestVerify(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Unix(1_800_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	body := []byte(`{"type":1}`)
	sig := hex.EncodeToString(ed25519.Sign(priv, append([]byte(ts), body...)))
	key := hex.EncodeToString(pub)

	if err := Verify(key, sig, ts, body, now); err != nil {
		t.Fatalf("正しい署名: %v", err)
	}
	if err := Verify(key, sig, ts, []byte(`{"type":2}`), now); err == nil {
		t.Error("本文が書き換えられたら拒否する")
	}
	if err := Verify(key, sig, ts, body, now.Add(10*time.Minute)); err == nil {
		t.Error("古いタイムスタンプは拒否する（リプレイ攻撃の防止）")
	}
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	if err := Verify(hex.EncodeToString(other), sig, ts, body, now); err == nil {
		t.Error("別の鍵では拒否する")
	}
	if err := Verify(key, "zz", ts, body, now); err == nil {
		t.Error("形式が不正な署名は拒否する")
	}
}

func TestInteractionParsing(t *testing.T) {
	raw := `{"type":2,"token":"tok","member":{"user":{"id":"42"}},"data":{"name":"pause","options":[{"name":"hours","value":3},{"name":"note","value":"試験"}]}}`
	var in Interaction
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatal(err)
	}
	if in.UserID() != "42" || in.Data.Name != "pause" || in.StringOption("note") != "試験" {
		t.Fatalf("%+v", in)
	}
	if h, ok := in.NumberOption("hours"); !ok || h != 3 {
		t.Fatalf("hours=%v", h)
	}
}

func TestMessageBlocksMentions(t *testing.T) {
	b, _ := json.Marshal(Message("@everyone こんにちは", true))
	var got map[string]any
	json.Unmarshal(b, &got)
	data := got["data"].(map[string]any)
	if data["flags"].(float64) != FlagEphemeral {
		t.Error("ephemeral フラグ")
	}
	if parse := data["allowed_mentions"].(map[string]any)["parse"].([]any); len(parse) != 0 {
		t.Error("メンションを無効にする")
	}
}
