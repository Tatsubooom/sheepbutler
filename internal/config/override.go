package config

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Overridable はチャットから変更できる設定項目と、その値の種類。
// ここに無い項目（志望企業・LLM のモデルなど）は config.yaml でしか変えられない。
var Overridable = map[string]string{
	"digest.times":                  "時刻の一覧（例: [\"07:00\", \"21:00\"]）",
	"digest.upcoming_days":          "整数（日。まとめ報告に載せる、この先の予定の日数。0〜14）",
	"notify.quiet_hours":            "[開始, 終了] の時刻（例: [\"00:00\", \"07:00\"]、無効にするなら []）",
	"notify.max_urgent_per_hour":    "整数",
	"notify.max_urgent_per_day":     "整数",
	"notify.reminder_hours":         "整数の一覧（例: [72, 24]）",
	"triage.thresholds.urgent":      "0〜1 の数",
	"triage.thresholds.digest":      "0〜1 の数",
	"triage.thresholds.bulk_digest": "0〜1 の数（一斉配信をまとめ報告に載せるしきい値）",
	"triage.weights.deadline":       "0 以上の数",
	"triage.weights.importance":     "0 以上の数",
	"triage.weights.interest":       "0 以上の数",
	"llm.daily_budget_usd":          "0 以上の数（USD）",
	"jobhunt.stale_after_days":      "整数（日）",
}

// OverridableKeys は変更できる項目の名前を並べて返す。
func OverridableKeys() []string {
	keys := make([]string, 0, len(Overridable))
	for k := range Overridable {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ApplyOverrides は base に上書き（"triage.thresholds.urgent" → 値）を重ねた設定を返す。
// 変更できない項目や、検証に通らない値はエラーにする（その場合 base は変わらない）。
func ApplyOverrides(base Config, overrides map[string]any) (Config, error) {
	if len(overrides) == 0 {
		return base, nil
	}
	b, err := yaml.Marshal(base)
	if err != nil {
		return base, err
	}
	var tree map[string]any
	if err := yaml.Unmarshal(b, &tree); err != nil {
		return base, err
	}
	for key, v := range overrides {
		if _, ok := Overridable[key]; !ok {
			return base, fmt.Errorf("%q はチャットからは変更できません", key)
		}
		if err := setPath(tree, strings.Split(key, "."), v); err != nil {
			return base, fmt.Errorf("%s: %w", key, err)
		}
	}
	b, err = yaml.Marshal(tree)
	if err != nil {
		return base, err
	}
	var out Config
	if err := yaml.Unmarshal(b, &out); err != nil {
		return base, fmt.Errorf("値の形式が合いません: %w", err)
	}
	if err := out.Validate(); err != nil {
		return base, err
	}
	return out, nil
}

func setPath(tree map[string]any, path []string, v any) error {
	cur := tree
	for _, p := range path[:len(path)-1] {
		next, ok := cur[p].(map[string]any)
		if !ok {
			return fmt.Errorf("項目 %q が見つかりません", p)
		}
		cur = next
	}
	cur[path[len(path)-1]] = v
	return nil
}
