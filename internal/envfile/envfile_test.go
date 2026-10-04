package envfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(p, []byte("# comment\nSB_A=1\nSB_B=\"quoted\"\nSB_C=keep\n\nSB_D=a=b\n"), 0o600)
	t.Setenv("SB_C", "already")
	os.Unsetenv("SB_A")
	os.Unsetenv("SB_B")
	os.Unsetenv("SB_D")
	t.Cleanup(func() { os.Unsetenv("SB_A"); os.Unsetenv("SB_B"); os.Unsetenv("SB_D") })

	if err := Load(p); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"SB_A": "1", "SB_B": "quoted", "SB_C": "already", "SB_D": "a=b"} {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

func TestLoadMissingFile(t *testing.T) {
	if err := Load(filepath.Join(t.TempDir(), "none")); err != nil {
		t.Fatalf("存在しないファイルはエラーにしない: %v", err)
	}
}

func TestLoadInvalidLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(p, []byte("NOEQUALS\n"), 0o600)
	if err := Load(p); err == nil {
		t.Fatal("不正な行でエラーになるべき")
	}
}
