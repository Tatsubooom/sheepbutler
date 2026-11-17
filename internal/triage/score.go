// Package triage はメールの必要度を判定する。
//
// LLM には判定材料の抜き出し（llm.Extraction）だけをさせ、必要度の計算と振り分けはこのパッケージのコードで行う。
// こうすると重みやしきい値を設定で変えられ、判定理由も説明できる。
package triage

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/Tatsubooom/sheepbutler/internal/config"
	"github.com/Tatsubooom/sheepbutler/internal/llm"
	"github.com/Tatsubooom/sheepbutler/internal/model"
)

// Mail は判定に使うメールの文面。
type Mail struct {
	From    string
	Subject string
	Body    string // 本文の冒頭（取得できなければ抜粋）
	Source  string // 転送元ラベル
}

// Score は判定材料から必要度を計算する。ex が nil ならルールだけで判定する（予算超過や LLM の失敗時）。
//
//	必要度 = (w期限×期限 + w重要×重要度 + w興味×興味度) ÷ (w期限 + w重要 + w興味)
//
// 重みの合計で割るので、重みを変えてもしきい値（0〜1）の意味は変わらない。
// その後、強制ルール（詐欺の疑い、選考の個別連絡、興味なしの企業）で段階を上書きする。
func Score(cfg config.Config, m Mail, ex *llm.Extraction, now time.Time) model.TriageResult {
	t := cfg.Triage
	var reasons []string
	text := m.Subject + "\n" + m.Body

	// 期限
	d := 0.0
	if ex != nil {
		if dl, ok := ex.DeadlineTime(); ok {
			left := dl.Sub(now)
			d = deadlineScore(t, left)
			if left > 0 {
				reasons = append(reasons, "締切まで"+humanDuration(left))
			}
		}
	}

	// 重要度
	imp := 0.0
	if mk := findMarker(t.ImportanceMarkers, text); mk != "" {
		imp = t.MarkerScore
		reasons = append(reasons, mk)
	}
	if ex != nil {
		imp = math.Max(imp, ex.Importance)
		if ex.MailKind == llm.KindBulk {
			imp *= t.BulkDiscount
			reasons = append(reasons, "一斉配信")
		}
	}

	// 興味度
	interest := 0.0
	company, known := config.Company{}, false
	if ex != nil {
		company, known = cfg.FindCompany(ex.Company)
	}
	if known {
		interest = config.InterestLevels[company.Interest]
		if company.Interest != "none" {
			reasons = append(reasons, "志望度"+company.Interest)
		}
	} else {
		if kw, v := findKeyword(cfg.JobHunt.Keywords, text); kw != "" {
			interest = v
			reasons = append(reasons, kw)
		}
		if ex != nil {
			interest = math.Max(interest, ex.InterestEstimate)
		}
	}

	w := t.Weights
	score := (w.Deadline*d + w.Importance*imp + w.Interest*interest) / (w.Deadline + w.Importance + w.Interest)
	score = math.Round(score*1000) / 1000

	tier := model.TierRecord
	switch {
	case score >= t.Thresholds.Urgent:
		tier = model.TierUrgent
	case score >= t.Thresholds.Digest:
		tier = model.TierDigest
	}

	// 強制ルール（点数より優先）
	if ex != nil {
		// 対応が要るか、選考が動いた（日程調整・結果など）か。どちらでもなければ即時にはしない。
		needsAttention := !t.UrgentNeedsAction || ex.ActionRequired
		switch {
		case known && company.Interest == "none":
			tier = model.TierRecord
			reasons = append(reasons, "興味なしの企業")
		case ex.MailKind == llm.KindSelection:
			// 受付完了の連絡は選考が動いたとは見なさない（日程調整・結果・書類の依頼などだけ）。
			moved := ex.SelectionEvent != llm.EventNone && ex.SelectionEvent != llm.EventEntryConfirmed
			if needsAttention || moved {
				tier = model.TierUrgent
			} else {
				tier = model.TierDigest // 受付完了・アンケートなど
			}
			reasons = append([]string{"選考の個別連絡"}, reasons...)
		case ex.MailKind == llm.KindBulk:
			// 一斉配信は即時にせず、点数が高いものだけまとめ報告に載せる。
			tier = model.TierRecord
			if score >= t.Thresholds.BulkDigest {
				tier = model.TierDigest
			}
		case tier == model.TierUrgent && !needsAttention:
			tier = model.TierDigest
			reasons = append(reasons, "対応不要")
		case tier == model.TierUrgent && t.UrgentNeedsAction && ex.MailKind == llm.KindEvent && !known:
			// 説明会・イベントの案内は、志望企業として登録した会社のものだけ即時にする（他はまとめ報告で十分）。
			tier = model.TierDigest
		case ex.ActionRequired && (ex.MailKind == llm.KindUniversity || ex.MailKind == llm.KindOther):
			// 大学の手続きなど、対応が要る個別の連絡は点数が低くても記録のみにはしない。
			tier = maxTier(tier, model.TierDigest)
		}
		// 詐欺の疑いは警告として必ず目に入る段階にする。ただし他のルールで決まった段階より下げはしない
		// （選考連絡が誤って詐欺扱いされても埋もれないように）。
		if ex.PhishingSuspect {
			tier = maxTier(tier, model.Tier(t.PhishingTier))
			reasons = append([]string{"詐欺の疑い"}, reasons...)
		}
	}

	method := model.MethodLLM
	if ex == nil {
		method = model.MethodRules
	}
	return model.TriageResult{
		Tier: tier, Score: score, Deadline: d, Importance: round(imp), Interest: round(interest),
		Reasons: reasons, Method: method, At: now,
	}
}

// Excluded は除外リストに当たるかを返す。当たれば理由を返す。
func Excluded(cfg config.Config, m Mail) (string, bool) {
	ex := cfg.Triage.Exclude
	for _, s := range ex.Sources {
		if s == m.Source {
			return "除外: 転送元 " + s, true
		}
	}
	from := strings.ToLower(m.From)
	for _, s := range ex.Senders {
		if s != "" && strings.Contains(from, strings.ToLower(s)) {
			return "除外: 差出人", true
		}
	}
	for _, k := range ex.SubjectKeywords {
		if k != "" && strings.Contains(m.Subject, k) {
			return "除外: 件名「" + k + "」", true
		}
	}
	return "", false
}

func deadlineScore(t config.Triage, left time.Duration) float64 {
	if left <= 0 {
		return 0 // 締切を過ぎている
	}
	h := left.Hours()
	for _, s := range t.DeadlineSteps {
		if h <= s.WithinHours {
			return s.Score
		}
	}
	return t.DeadlineFar
}

func findMarker(markers []string, text string) string {
	for _, mk := range markers {
		if mk != "" && strings.Contains(text, mk) {
			return mk
		}
	}
	return ""
}

// findKeyword は本文に含まれるキーワードのうち、最も点数の高いものを返す。
func findKeyword(kws map[string]float64, text string) (string, float64) {
	best, bestV := "", 0.0
	for k, v := range kws {
		if k != "" && strings.Contains(text, k) && (v > bestV || (v == bestV && k < best)) {
			best, bestV = k, v
		}
	}
	return best, bestV
}

func humanDuration(d time.Duration) string {
	if d < time.Hour {
		return fmt.Sprintf("%d分", int(d.Minutes()))
	}
	if d < 48*time.Hour {
		return fmt.Sprintf("%d時間", int(d.Hours()))
	}
	return fmt.Sprintf("%d日", int(d.Hours()/24))
}

func round(v float64) float64 { return math.Round(v*1000) / 1000 }

var tierRank = map[model.Tier]int{model.TierRecord: 0, model.TierDigest: 1, model.TierUrgent: 2}

func maxTier(a, b model.Tier) model.Tier {
	if tierRank[b] > tierRank[a] {
		return b
	}
	return a
}
