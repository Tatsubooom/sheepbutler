// Package config は必要度判定などの設定（config.yaml）を読み込む。
// 志望企業やプロフィールなどの個人情報を含むため、config.yaml は Git に入れず、
// Cloud Run には Secret Manager からファイルとして渡す。
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Triage  Triage  `yaml:"triage"`
	JobHunt JobHunt `yaml:"jobhunt"`
	LLM     LLM     `yaml:"llm"`
	Notify  Notify  `yaml:"notify"`
	Digest  Digest  `yaml:"digest"`
}

// Notify は即時通知とリマインドの設定。時刻はすべて日本時間の "HH:MM"。
type Notify struct {
	// QuietHours の間は即時通知を送らず、次のまとめ報告に回す。[開始, 終了]。日をまたいでもよい。
	QuietHours       []string `yaml:"quiet_hours"`
	MaxUrgentPerHour int      `yaml:"max_urgent_per_hour"`
	MaxUrgentPerDay  int      `yaml:"max_urgent_per_day"`
	// ReminderHours は締切の何時間前にリマインドするか。
	ReminderHours []int `yaml:"reminder_hours"`
	// LineMonthlyLimit は LINE の月間送信数の上限。近づいたらまとめ報告だけにする。
	LineMonthlyLimit int `yaml:"line_monthly_limit"`
}

// Digest はまとめ報告の設定。
type Digest struct {
	Times []string `yaml:"times"`
	// DeadlineDays は何日以内の締切を載せるか。
	DeadlineDays int `yaml:"deadline_days"`
	// MaxItems はまとめ待ちのメールを何通まで載せるか（超えた分は件数だけ）。
	MaxItems int `yaml:"max_items"`
}

// ParseClock は "HH:MM" を 0 時からの分に直す。
func ParseClock(s string) (int, error) {
	var h, m int
	if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, fmt.Errorf("時刻 %q は HH:MM で指定してください", s)
	}
	return h*60 + m, nil
}

type Triage struct {
	Weights    Weights    `yaml:"weights"`
	Thresholds Thresholds `yaml:"thresholds"`
	// DeadlineSteps は「締切までの残り時間 → 期限の点数」。within_hours の小さい順に評価する。
	DeadlineSteps []DeadlineStep `yaml:"deadline_steps"`
	// DeadlineFar は DeadlineSteps のどれにも当たらない（遠い）締切の点数。
	DeadlineFar float64 `yaml:"deadline_far"`
	// ImportanceMarkers は件名・本文にあれば重要度を MarkerScore 以上にする目印。
	ImportanceMarkers []string `yaml:"importance_markers"`
	MarkerScore       float64  `yaml:"marker_score"`
	// BulkDiscount は一斉配信（ナビサイトの広告など）の重要度に掛ける係数。
	BulkDiscount float64 `yaml:"bulk_discount"`
	// PhishingTier は詐欺の疑いがあるメールの最低の段階（urgent / digest / record）。
	// LLM の詐欺判定は誤検知があるため、初期値は digest（警告付きでまとめ報告）。
	PhishingTier string  `yaml:"phishing_tier"`
	Exclude      Exclude `yaml:"exclude"`
}

type Weights struct {
	Deadline   float64 `yaml:"deadline"`
	Importance float64 `yaml:"importance"`
	Interest   float64 `yaml:"interest"`
}

type Thresholds struct {
	Urgent float64 `yaml:"urgent"`
	Digest float64 `yaml:"digest"`
}

type DeadlineStep struct {
	WithinHours float64 `yaml:"within_hours"`
	Score       float64 `yaml:"score"`
}

// Exclude に当たるメールは LLM に送らず「記録のみ」にする。
type Exclude struct {
	Senders         []string `yaml:"senders"`
	SubjectKeywords []string `yaml:"subject_keywords"`
	Sources         []string `yaml:"sources"` // 転送元ラベル単位で除外する
}

type JobHunt struct {
	// Profile は興味の傾向。未登録の企業の興味度を LLM が推定するときに使う。
	Profile   string             `yaml:"profile"`
	Companies []Company          `yaml:"companies"`
	Keywords  map[string]float64 `yaml:"keywords"`
}

type Company struct {
	Name     string   `yaml:"name"`
	Aliases  []string `yaml:"aliases"`
	Interest string   `yaml:"interest"` // S / A / B / C / none
}

type LLM struct {
	Model          string  `yaml:"model"`
	MaxInputChars  int     `yaml:"max_input_chars"`
	DailyBudgetUSD float64 `yaml:"daily_budget_usd"`
	// StructuredOutput が true なら構造化出力（JSON スキーマ）で形式を保証する。1回あたり約450トークン増える。
	StructuredOutput bool `yaml:"structured_output"`
}

// InterestLevels は志望度の記号と興味度の対応。
var InterestLevels = map[string]float64{"S": 1.0, "A": 0.75, "B": 0.5, "C": 0.25, "none": 0}

// Default は設定ファイルが無い項目に使う初期値。数値は運用しながら調整する前提の仮の値。
func Default() Config {
	return Config{
		Triage: Triage{
			Weights:    Weights{Deadline: 0.4, Importance: 0.25, Interest: 0.35},
			Thresholds: Thresholds{Urgent: 0.7, Digest: 0.3},
			DeadlineSteps: []DeadlineStep{
				{WithinHours: 24, Score: 1.0},
				{WithinHours: 72, Score: 0.7},
				{WithinHours: 168, Score: 0.4},
			},
			DeadlineFar:       0.2,
			ImportanceMarkers: []string{"【重要】", "【要返信】", "【必読】", "リマインド", "再送", "締切", "〆切"},
			MarkerScore:       0.7,
			BulkDiscount:      0.5,
			PhishingTier:      "digest",
		},
		LLM: LLM{Model: "claude-haiku-4-5", MaxInputChars: 1500, DailyBudgetUSD: 0.5},
		Notify: Notify{
			QuietHours:       []string{"00:00", "07:00"},
			MaxUrgentPerHour: 5,
			MaxUrgentPerDay:  20,
			ReminderHours:    []int{72, 24},
			LineMonthlyLimit: 200,
		},
		Digest: Digest{Times: []string{"07:00", "21:00"}, DeadlineDays: 3, MaxItems: 10},
	}
}

// Load は path の YAML を読み、初期値に上書きする。path が空なら初期値だけを返す。
func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, cfg.Validate()
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("設定ファイルを読めません: %w", err)
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("設定ファイルの形式が不正です: %w", err)
	}
	return cfg, cfg.Validate()
}

// Validate は明らかに誤った設定を起動時に弾く。
func (c Config) Validate() error {
	var errs []error
	w := c.Triage.Weights
	if w.Deadline < 0 || w.Importance < 0 || w.Interest < 0 {
		errs = append(errs, errors.New("triage.weights に負の値は使えません"))
	}
	if w.Deadline+w.Importance+w.Interest == 0 {
		errs = append(errs, errors.New("triage.weights がすべて 0 です"))
	}
	t := c.Triage.Thresholds
	if t.Digest < 0 || t.Urgent <= t.Digest {
		errs = append(errs, fmt.Errorf("triage.thresholds は 0 <= digest < urgent にしてください（digest=%v, urgent=%v）", t.Digest, t.Urgent))
	}
	for i := 1; i < len(c.Triage.DeadlineSteps); i++ {
		if c.Triage.DeadlineSteps[i].WithinHours <= c.Triage.DeadlineSteps[i-1].WithinHours {
			errs = append(errs, errors.New("triage.deadline_steps は within_hours の小さい順に並べてください"))
			break
		}
	}
	if d := c.Triage.BulkDiscount; d < 0 || d > 1 {
		errs = append(errs, errors.New("triage.bulk_discount は 0〜1 にしてください"))
	}
	switch c.Triage.PhishingTier {
	case "urgent", "digest", "record":
	default:
		errs = append(errs, fmt.Errorf("triage.phishing_tier は urgent / digest / record のどれかにしてください: %q", c.Triage.PhishingTier))
	}
	for _, co := range c.JobHunt.Companies {
		if _, ok := InterestLevels[co.Interest]; !ok {
			errs = append(errs, fmt.Errorf("jobhunt.companies の %q: interest は S/A/B/C/none のどれかにしてください", co.Name))
		}
	}
	if c.LLM.Model == "" || c.LLM.MaxInputChars <= 0 || c.LLM.DailyBudgetUSD < 0 {
		errs = append(errs, errors.New("llm の model / max_input_chars / daily_budget_usd を確認してください"))
	}
	n := c.Notify
	if len(n.QuietHours) != 0 && len(n.QuietHours) != 2 {
		errs = append(errs, errors.New("notify.quiet_hours は [開始, 終了] の2つで指定してください（無効にするなら []）"))
	}
	for _, s := range append(append([]string{}, n.QuietHours...), c.Digest.Times...) {
		if _, err := ParseClock(s); err != nil {
			errs = append(errs, err)
		}
	}
	if n.MaxUrgentPerHour < 0 || n.MaxUrgentPerDay < 0 || n.LineMonthlyLimit < 0 {
		errs = append(errs, errors.New("notify の上限に負の値は使えません"))
	}
	for _, h := range n.ReminderHours {
		if h <= 0 {
			errs = append(errs, errors.New("notify.reminder_hours は正の時間で指定してください"))
		}
	}
	if c.Digest.DeadlineDays < 0 || c.Digest.MaxItems <= 0 {
		errs = append(errs, errors.New("digest.deadline_days は 0 以上、max_items は 1 以上にしてください"))
	}
	return errors.Join(errs...)
}

// FindCompany は企業名（表記揺れを含む）から登録済みの企業を探す。
func (c Config) FindCompany(name string) (Company, bool) {
	n := normalizeCompany(name)
	if n == "" {
		return Company{}, false
	}
	for _, co := range c.JobHunt.Companies {
		for _, cand := range append([]string{co.Name}, co.Aliases...) {
			if normalizeCompany(cand) == n {
				return co, true
			}
		}
	}
	return Company{}, false
}

// normalizeCompany は「株式会社」の有無や空白・大文字小文字の違いを吸収する。
func normalizeCompany(s string) string {
	s = strings.ToLower(s)
	for _, r := range []string{"株式会社", "(株)", "（株）", "有限会社", "合同会社", " ", "　"} {
		s = strings.ReplaceAll(s, r, "")
	}
	return s
}
