// Package guard は Google Cloud の予算通知を受け取り、費用が予算に近づいたら SheepButler を止める。
//
//   - 予算の SoftRatio（既定 50%）以上: SheepButler だけを止める（ジョブの一時停止、Pub/Sub の push 解除）。データは残り、再デプロイで再開できる
//   - 予算の HardRatio（既定 100%）以上: プロジェクトから課金を外し、全サービスを止める
//
// 予算の費用データは数時間〜1日遅れて届くため、止まるまでにわずかに超える可能性がある。
package guard

import (
	"context"
	"fmt"
	"log/slog"
)

// Notification は予算アラートの Pub/Sub メッセージ本文。
// https://cloud.google.com/billing/docs/how-to/budgets-programmatic-notifications
type Notification struct {
	BudgetDisplayName      string  `json:"budgetDisplayName"`
	CostAmount             float64 `json:"costAmount"`
	BudgetAmount           float64 `json:"budgetAmount"`
	CurrencyCode           string  `json:"currencyCode"`
	AlertThresholdExceeded float64 `json:"alertThresholdExceeded"`
	CostIntervalStart      string  `json:"costIntervalStart"`
}

type Action int

const (
	None Action = iota
	SoftStop
	HardStop
)

func (a Action) String() string {
	return [...]string{"none", "soft-stop", "hard-stop"}[a]
}

// Decide は費用と予算の比率から取るべき行動を決める。
func Decide(n Notification, softRatio, hardRatio float64) Action {
	if n.BudgetAmount <= 0 {
		return None
	}
	r := n.CostAmount / n.BudgetAmount
	switch {
	case r >= hardRatio:
		return HardStop
	case r >= softRatio:
		return SoftStop
	default:
		return None
	}
}

// Stopper は実際に止める操作。changed は今回の呼び出しで状態が変わったか（通知の重複を防ぐため）。
type Stopper interface {
	StopApp(ctx context.Context) (changed bool, err error)
	DisableBilling(ctx context.Context) (changed bool, err error)
}

// Notifier は停止を知らせる先。nil なら知らせない。
type Notifier interface {
	Send(ctx context.Context, text string) error
}

type Guard struct {
	Stopper   Stopper
	Notifier  Notifier
	SoftRatio float64
	HardRatio float64
	Log       *slog.Logger
}

// Handle は予算通知を1件処理する。止める操作に失敗したらエラーを返す（Pub/Sub に再送させる）。
func (g *Guard) Handle(ctx context.Context, n Notification) (Action, error) {
	act := Decide(n, g.SoftRatio, g.HardRatio)
	g.Log.Info("予算通知", "action", act.String(),
		"cost", n.CostAmount, "budget", n.BudgetAmount, "currency", n.CurrencyCode)

	switch act {
	case SoftStop:
		changed, err := g.Stopper.StopApp(ctx)
		if err != nil {
			return act, fmt.Errorf("アプリの停止: %w", err)
		}
		if changed {
			g.notify(ctx, fmt.Sprintf(
				"【SheepButler 停止】今月の Google Cloud の費用が予算の %.0f%% に達したため（%.0f / %.0f %s）、SheepButler を一時停止しました。データは残っています。原因を確認し、scripts/deploy.sh を再実行すると再開します。",
				100*n.CostAmount/n.BudgetAmount, n.CostAmount, n.BudgetAmount, n.CurrencyCode))
		}
	case HardStop:
		// 課金を外すとこのサービス自身も止まるので、先に知らせてアプリも止めておく。
		g.notify(ctx, fmt.Sprintf(
			"【SheepButler 緊急停止】今月の Google Cloud の費用が予算に達したため（%.0f / %.0f %s）、プロジェクトの課金を外して全サービスを止めます。再開するには請求先アカウントを再リンクし、scripts/deploy.sh を再実行してください。",
			n.CostAmount, n.BudgetAmount, n.CurrencyCode))
		if _, err := g.Stopper.StopApp(ctx); err != nil {
			g.Log.Error("アプリの停止に失敗（課金の解除は続行）", "err", err)
		}
		if _, err := g.Stopper.DisableBilling(ctx); err != nil {
			return act, fmt.Errorf("課金の解除: %w", err)
		}
	}
	return act, nil
}

func (g *Guard) notify(ctx context.Context, text string) {
	if g.Notifier == nil {
		return
	}
	if err := g.Notifier.Send(ctx, text); err != nil {
		g.Log.Error("停止の通知に失敗", "err", err)
	}
}
