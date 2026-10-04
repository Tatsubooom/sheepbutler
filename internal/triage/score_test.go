package triage

import (
	"slices"
	"testing"
	"time"

	"github.com/Tatsubooom/sheepbutler/internal/config"
	"github.com/Tatsubooom/sheepbutler/internal/llm"
	"github.com/Tatsubooom/sheepbutler/internal/model"
)

var (
	jst = time.FixedZone("JST", 9*3600)
	now = time.Date(2026, 10, 4, 12, 0, 0, 0, jst)
)

func testConfig() config.Config {
	c := config.Default()
	c.JobHunt.Companies = []config.Company{
		{Name: "株式会社第一志望", Interest: "S"},
		{Name: "興味なし商事", Interest: "none"},
	}
	c.JobHunt.Keywords = map[string]float64{"早期選考": 0.9, "インターン": 0.6}
	return c
}

func deadlineIn(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }

func TestScoreTiers(t *testing.T) {
	cfg := testConfig()
	tests := []struct {
		name string
		mail Mail
		ex   *llm.Extraction
		want model.Tier
	}{
		{
			"志望度Sの説明会で締切が明日 → 即時",
			Mail{Subject: "説明会のご案内"},
			&llm.Extraction{MailKind: llm.KindEvent, Company: "第一志望", Deadline: deadlineIn(20 * time.Hour), Importance: 0.6},
			model.TierUrgent,
		},
		{
			"ナビサイトの広告は【重要】でも記録のみ",
			Mail{Subject: "【重要】今すぐエントリー"},
			&llm.Extraction{MailKind: llm.KindBulk, Importance: 0.3, InterestEstimate: 0.1},
			model.TierRecord,
		},
		{
			"大学からの手続き（締切5日後）→ まとめ",
			Mail{Subject: "履修登録の確認"},
			&llm.Extraction{MailKind: llm.KindUniversity, Deadline: deadlineIn(5 * 24 * time.Hour), Importance: 0.7, ActionRequired: true},
			model.TierDigest,
		},
		{
			"選考の個別連絡は点数に関係なく即時",
			Mail{Subject: "面接日程のご連絡"},
			&llm.Extraction{MailKind: llm.KindSelection, Company: "知らない会社", Importance: 0.2},
			model.TierUrgent,
		},
		{
			"興味なしの企業は選考連絡でも記録のみ",
			Mail{Subject: "面接のご案内"},
			&llm.Extraction{MailKind: llm.KindSelection, Company: "興味なし商事", Importance: 0.9, Deadline: deadlineIn(time.Hour)},
			model.TierRecord,
		},
		{
			"詐欺の疑いは警告付きでまとめ（初期設定）",
			Mail{Subject: "アカウント停止のお知らせ"},
			&llm.Extraction{MailKind: llm.KindOther, PhishingSuspect: true},
			model.TierDigest,
		},
		{
			"詐欺の疑いでも選考連絡は即時のまま（段階を下げない）",
			Mail{Subject: "面接日程"},
			&llm.Extraction{MailKind: llm.KindSelection, PhishingSuspect: true},
			model.TierUrgent,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Score(cfg, tt.mail, tt.ex, now)
			if got.Tier != tt.want {
				t.Errorf("tier = %s, want %s（score=%v D=%v I=%v N=%v reasons=%v）",
					got.Tier, tt.want, got.Score, got.Deadline, got.Importance, got.Interest, got.Reasons)
			}
		})
	}
}

func TestScoreComponents(t *testing.T) {
	cfg := testConfig()

	t.Run("締切の段階", func(t *testing.T) {
		for _, c := range []struct {
			left time.Duration
			want float64
		}{
			{-time.Hour, 0}, {10 * time.Hour, 1.0}, {48 * time.Hour, 0.7}, {6 * 24 * time.Hour, 0.4}, {30 * 24 * time.Hour, 0.2},
		} {
			got := Score(cfg, Mail{}, &llm.Extraction{Deadline: deadlineIn(c.left)}, now)
			if got.Deadline != c.want {
				t.Errorf("残り %v: D = %v, want %v", c.left, got.Deadline, c.want)
			}
		}
	})

	t.Run("目印は重要度の下限になる", func(t *testing.T) {
		got := Score(cfg, Mail{Subject: "【要返信】確認"}, &llm.Extraction{Importance: 0.2}, now)
		if got.Importance != 0.7 || !slices.Contains(got.Reasons, "【要返信】") {
			t.Errorf("I=%v reasons=%v", got.Importance, got.Reasons)
		}
	})

	t.Run("キーワードは興味度の下限になる", func(t *testing.T) {
		got := Score(cfg, Mail{Body: "早期選考のご案内"}, &llm.Extraction{InterestEstimate: 0.3}, now)
		if got.Interest != 0.9 {
			t.Errorf("N=%v", got.Interest)
		}
	})

	t.Run("重みを変えてもしきい値の意味は保たれる", func(t *testing.T) {
		c := testConfig()
		c.Triage.Weights = config.Weights{Deadline: 4, Importance: 2.5, Interest: 3.5} // 10 倍
		a := Score(cfg, Mail{}, &llm.Extraction{Importance: 0.5, InterestEstimate: 0.5}, now)
		b := Score(c, Mail{}, &llm.Extraction{Importance: 0.5, InterestEstimate: 0.5}, now)
		if a.Score != b.Score {
			t.Errorf("%v != %v", a.Score, b.Score)
		}
	})
}

func TestPhishingTierConfigurable(t *testing.T) {
	cfg := testConfig()
	cfg.Triage.PhishingTier = "urgent"
	got := Score(cfg, Mail{}, &llm.Extraction{MailKind: llm.KindOther, PhishingSuspect: true}, now)
	if got.Tier != model.TierUrgent || got.Reasons[0] != "詐欺の疑い" {
		t.Fatalf("%+v", got)
	}
}

func TestScoreRulesOnly(t *testing.T) {
	got := Score(testConfig(), Mail{Subject: "【重要】奨学金の手続き"}, nil, now)
	if got.Method != model.MethodRules || got.Importance != 0.7 || got.Tier != model.TierRecord {
		t.Errorf("ルールのみ: %+v", got)
	}
}

func TestExcluded(t *testing.T) {
	cfg := testConfig()
	cfg.Triage.Exclude = config.Exclude{
		Senders: []string{"grades@example.ac.jp"}, SubjectKeywords: []string{"成績"}, Sources: []string{"個人2"},
	}
	for _, m := range []Mail{
		{From: "教務 <Grades@example.ac.jp>"},
		{Subject: "前期成績のお知らせ"},
		{Source: "個人2"},
	} {
		if _, ok := Excluded(cfg, m); !ok {
			t.Errorf("除外されるべき: %+v", m)
		}
	}
	if _, ok := Excluded(cfg, Mail{From: "a@b.c", Subject: "面接", Source: "大学"}); ok {
		t.Error("除外されないべき")
	}
}
