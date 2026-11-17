// review は判定済みのメールを一覧表示し、重みやしきい値の調整を手伝う。
//
//	go run ./cmd/review [-days 14]      直近の判定結果の一覧（新しい順）
//	go run ./cmd/review -rescore        今の config.yaml で点数を計算し直し、変わったものを表示
//	                                    （保存済みの抽出結果を使うので LLM は呼ばない＝費用ゼロ）
//	go run ./cmd/review -rescore -apply 計算し直した結果を Firestore に保存する
//	go run ./cmd/review -retriage       判定済みのメールを未判定に戻す（LLM への指示を変えたとき。その後 server -once triage）
//
// Firestore への接続には `gcloud auth application-default login` が必要。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Tatsubooom/sheepbutler/internal/config"
	"github.com/Tatsubooom/sheepbutler/internal/envfile"
	"github.com/Tatsubooom/sheepbutler/internal/model"
	"github.com/Tatsubooom/sheepbutler/internal/store"
	"github.com/Tatsubooom/sheepbutler/internal/triage"
)

func main() {
	limit := flag.Int("limit", 100, "表示する件数")
	rescore := flag.Bool("rescore", false, "今の config.yaml で点数を計算し直す")
	apply := flag.Bool("apply", false, "-rescore の結果を保存する")
	days := flag.Int("days", 14, "直近何日に受信したメールを対象にするか")
	retriage := flag.Bool("retriage", false, "判定済みのメールを未判定に戻す（LLM への指示を変えたときに使う）")
	flag.Parse()

	if err := envfile.Load(".env"); err != nil {
		log.Fatal(err)
	}
	cfgPath := os.Getenv("CONFIG_PATH")
	if cfgPath == "" {
		cfgPath = "config.yaml"
	}
	if _, err := os.Stat(cfgPath); err != nil {
		cfgPath = ""
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatal(err)
	}
	jst, _ := time.LoadLocation("Asia/Tokyo")

	ctx := context.Background()
	fs, err := store.NewFirestore(ctx, os.Getenv("GOOGLE_CLOUD_PROJECT"))
	if err != nil {
		log.Fatal(err)
	}
	defer fs.Close()

	// 直近 days 日に受信した判定済みのメール（新しい順）。
	all, err := fs.ListReceivedSince(ctx, time.Now().AddDate(0, 0, -*days))
	if err != nil {
		log.Fatal(err)
	}
	var msgs []*model.Message
	for _, m := range all {
		if m.Status == model.StatusTriaged {
			msgs = append(msgs, m)
		}
	}
	sort.Slice(msgs, func(i, j int) bool { return msgs[i].ReceivedAt.After(msgs[j].ReceivedAt) })
	if len(msgs) > *limit {
		msgs = msgs[:*limit]
	}

	if *retriage {
		for _, m := range msgs {
			if err := fs.ResetToPending(ctx, m.ID); err != nil {
				log.Fatal(err)
			}
		}
		fmt.Printf("%d 件を未判定に戻しました。go run ./cmd/server -once triage で判定し直してください（LLM の費用がかかります）。\n", len(msgs))
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer w.Flush()

	if !*rescore {
		fmt.Fprintln(w, "受信\t転送元\t段階\t点数\tD/I/N\t種類\t件名\t理由")
		count := map[model.Tier]int{}
		for _, m := range msgs {
			t := m.Triage
			if t == nil {
				continue
			}
			count[t.Tier]++
			fmt.Fprintf(w, "%s\t%s\t%s\t%.2f\t%.1f/%.1f/%.1f\t%s\t%s\t%s\n",
				m.ReceivedAt.In(jst).Format("01/02 15:04"), m.Source, tierLabel(t.Tier), t.Score,
				t.Deadline, t.Importance, t.Interest, kind(m), cut(m.Subject, 28), strings.Join(t.Reasons, "・"))
		}
		w.Flush()
		fmt.Printf("\n%d 件: 即時 %d / まとめ %d / 記録のみ %d\n",
			len(msgs), count[model.TierUrgent], count[model.TierDigest], count[model.TierRecord])
		return
	}

	// 保存済みの抽出結果で計算し直す。締切の点数は判定した時点を基準にする（今を基準にすると過去の締切が全部 0 になるため）。
	fmt.Fprintln(w, "受信\t件名\t旧段階\t新段階\t旧点数\t新点数\t新しい理由")
	changed := 0
	for _, m := range msgs {
		if m.Triage == nil || m.Triage.Method == model.MethodExcluded {
			continue
		}
		mail := triage.Mail{From: m.From, Subject: m.Subject, Body: m.Snippet, Source: m.Source}
		if reason, ok := triage.Excluded(cfg, mail); ok {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%.2f\t-\t%s\n", m.ReceivedAt.In(jst).Format("01/02 15:04"), cut(m.Subject, 28),
				tierLabel(m.Triage.Tier), tierLabel(model.TierRecord), m.Triage.Score, reason)
			changed++
			continue
		}
		res := triage.Score(cfg, mail, m.Extracted, m.Triage.At)
		if res.Tier == m.Triage.Tier && res.Score == m.Triage.Score {
			continue
		}
		changed++
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%.2f\t%.2f\t%s\n", m.ReceivedAt.In(jst).Format("01/02 15:04"), cut(m.Subject, 28),
			tierLabel(m.Triage.Tier), tierLabel(res.Tier), m.Triage.Score, res.Score, strings.Join(res.Reasons, "・"))
		if *apply {
			if err := fs.SaveTriage(ctx, m.ID, m.Extracted, res, false); err != nil {
				log.Fatal(err)
			}
		}
	}
	w.Flush()
	fmt.Printf("\n%d 件中 %d 件が変わります。", len(msgs), changed)
	if *apply {
		fmt.Println("保存しました。")
	} else if changed > 0 {
		fmt.Println("保存するには -apply を付けて実行してください。")
	} else {
		fmt.Println()
	}
}

func tierLabel(t model.Tier) string {
	switch t {
	case model.TierUrgent:
		return "即時"
	case model.TierDigest:
		return "まとめ"
	}
	return "記録"
}

func kind(m *model.Message) string {
	if m.Extracted == nil {
		return "-"
	}
	return string(m.Extracted.MailKind)
}

func cut(s string, n int) string {
	r := []rune(strings.ReplaceAll(s, "\t", " "))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}
