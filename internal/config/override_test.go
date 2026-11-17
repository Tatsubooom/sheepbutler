package config

import (
	"strings"
	"testing"
)

func TestApplyOverrides(t *testing.T) {
	base := Default()
	base.JobHunt.Companies = []Company{{Name: "A社", Interest: "S"}}

	got, err := ApplyOverrides(base, map[string]any{
		"digest.times":              []any{"06:30", "22:00"},
		"triage.thresholds.urgent":  0.8,
		"notify.max_urgent_per_day": 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Digest.Times[0] != "06:30" || got.Triage.Thresholds.Urgent != 0.8 || got.Notify.MaxUrgentPerDay != 10 {
		t.Fatalf("上書きが反映されない: %+v %+v", got.Digest, got.Triage.Thresholds)
	}
	if len(got.JobHunt.Companies) != 1 || got.LLM.Model != base.LLM.Model {
		t.Fatal("上書きしていない項目は保つ")
	}
	if base.Digest.Times[0] != "07:00" {
		t.Fatal("base は変えない")
	}
}

func TestApplyOverridesRejects(t *testing.T) {
	for name, ov := range map[string]map[string]any{
		"変更できない項目": {"llm.model": "claude-opus-5-5"},
		"しきい値の逆転":  {"triage.thresholds.urgent": 0.1},
		"時刻の形式":    {"digest.times": []any{"7時"}},
		"型が違う":     {"notify.max_urgent_per_day": "たくさん"},
		"負の重み":     {"triage.weights.interest": -1},
	} {
		if _, err := ApplyOverrides(Default(), ov); err == nil {
			t.Errorf("%s: エラーになるべき", name)
		} else if name == "変更できない項目" && !strings.Contains(err.Error(), "変更できません") {
			t.Errorf("%s: %v", name, err)
		}
	}
}
