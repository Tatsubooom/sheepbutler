package notify

import (
	"strings"
	"testing"
)

func TestSplit(t *testing.T) {
	if got := Split("短い", 10); len(got) != 1 || got[0] != "短い" {
		t.Fatalf("上限以下はそのまま: %q", got)
	}

	text := "一行目です\n二行目です\n三行目です\n"
	got := Split(text, 12)
	if len(got) != 2 || got[0] != "一行目です\n二行目です" || got[1] != "三行目です" {
		t.Fatalf("行の切れ目で分ける: %q", got)
	}

	long := strings.Repeat("あ", 25)
	got = Split(long, 10)
	if len(got) != 3 || len([]rune(got[0])) != 10 || len([]rune(got[2])) != 5 {
		t.Fatalf("長すぎる1行は途中で切る: %q", got)
	}
	for _, c := range Split(strings.Repeat("あいう\n", 100), 50) {
		if len([]rune(c)) > 50 {
			t.Fatalf("上限を超える塊がある: %d", len([]rune(c)))
		}
	}
}
