package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExampleConfigLoads(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatalf("config.example.yaml が読めない: %v", err)
	}
	if cfg.Triage.Weights.Deadline != 0.4 || len(cfg.JobHunt.Companies) != 1 {
		t.Fatalf("読み込み結果: %+v", cfg)
	}
}

func TestPartialConfigKeepsDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(p, []byte("triage:\n  thresholds: { urgent: 0.8, digest: 0.2 }\n"), 0o600)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Triage.Thresholds.Urgent != 0.8 || cfg.Triage.Weights.Deadline != 0.4 || cfg.LLM.Model == "" {
		t.Fatalf("書いた項目だけ上書きし、他は初期値: %+v", cfg)
	}
}

func TestValidate(t *testing.T) {
	bad := []struct {
		name string
		mod  func(*Config)
		want string
	}{
		{"しきい値の逆転", func(c *Config) { c.Triage.Thresholds = Thresholds{Urgent: 0.2, Digest: 0.5} }, "thresholds"},
		{"負の重み", func(c *Config) { c.Triage.Weights.Interest = -1 }, "weights"},
		{"志望度の誤記", func(c *Config) { c.JobHunt.Companies = []Company{{Name: "X", Interest: "SS"}} }, "interest"},
		{"詐欺の段階の誤記", func(c *Config) { c.Triage.PhishingTier = "high" }, "phishing_tier"},
		{"締切の並び順", func(c *Config) {
			c.Triage.DeadlineSteps = []DeadlineStep{{WithinHours: 72}, {WithinHours: 24}}
		}, "deadline_steps"},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			c := Default()
			tt.mod(&c)
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q を含むエラー", err, tt.want)
			}
		})
	}
}

func TestFindCompany(t *testing.T) {
	c := Default()
	c.JobHunt.Companies = []Company{{Name: "株式会社資生堂", Aliases: []string{"SHISEIDO"}, Interest: "A"}}
	for _, name := range []string{"資生堂", "株式会社 資生堂", "（株）資生堂", "shiseido"} {
		if co, ok := c.FindCompany(name); !ok || co.Interest != "A" {
			t.Errorf("FindCompany(%q) が見つからない", name)
		}
	}
	if _, ok := c.FindCompany("トヨタ"); ok {
		t.Error("未登録の企業は見つからない")
	}
	if _, ok := c.FindCompany(""); ok {
		t.Error("空の企業名は見つからない")
	}
}

func TestParseClock(t *testing.T) {
	if m, err := ParseClock("07:30"); err != nil || m != 450 {
		t.Fatalf("got %d %v", m, err)
	}
	for _, bad := range []string{"25:00", "7時", "", "12:60"} {
		if _, err := ParseClock(bad); err == nil {
			t.Errorf("ParseClock(%q) はエラーになるべき", bad)
		}
	}
}

func TestValidateNotify(t *testing.T) {
	c := Default()
	c.Notify.QuietHours = []string{"00:00"}
	c.Digest.Times = []string{"7:00", "27:00"}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "quiet_hours") || !strings.Contains(err.Error(), "27:00") {
		t.Fatalf("err = %v", err)
	}
}
