package guard

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestDecide(t *testing.T) {
	tests := []struct {
		cost, budget float64
		want         Action
	}{
		{0, 100, None},
		{49, 100, None},
		{50, 100, SoftStop},
		{99, 100, SoftStop},
		{100, 100, HardStop},
		{150, 100, HardStop},
		{10, 0, None}, // 予算額が不正なら何もしない
	}
	for _, tt := range tests {
		if got := Decide(Notification{CostAmount: tt.cost, BudgetAmount: tt.budget}, 0.5, 1.0); got != tt.want {
			t.Errorf("cost=%v budget=%v: got %v, want %v", tt.cost, tt.budget, got, tt.want)
		}
	}
}

type fakeStopper struct {
	appStopped, billingOff bool
	appErr, billErr        error
}

func (f *fakeStopper) StopApp(context.Context) (bool, error) {
	if f.appErr != nil {
		return false, f.appErr
	}
	changed := !f.appStopped
	f.appStopped = true
	return changed, nil
}

func (f *fakeStopper) DisableBilling(context.Context) (bool, error) {
	if f.billErr != nil {
		return false, f.billErr
	}
	changed := !f.billingOff
	f.billingOff = true
	return changed, nil
}

type fakeNotifier struct{ sent []string }

func (f *fakeNotifier) Send(_ context.Context, text string) error {
	f.sent = append(f.sent, text)
	return nil
}

func newGuard(s *fakeStopper, n *fakeNotifier) *Guard {
	g := &Guard{Stopper: s, SoftRatio: 0.5, HardRatio: 1.0, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if n != nil {
		g.Notifier = n
	}
	return g
}

func TestHandleBelowBudgetDoesNothing(t *testing.T) {
	s, n := &fakeStopper{}, &fakeNotifier{}
	if _, err := newGuard(s, n).Handle(context.Background(), Notification{CostAmount: 10, BudgetAmount: 100}); err != nil {
		t.Fatal(err)
	}
	if s.appStopped || s.billingOff || len(n.sent) != 0 {
		t.Fatalf("予算の半分未満では何もしない: %+v %v", s, n.sent)
	}
}

func TestHandleSoftStopNotifiesOnce(t *testing.T) {
	s, n := &fakeStopper{}, &fakeNotifier{}
	g := newGuard(s, n)
	note := Notification{CostAmount: 60, BudgetAmount: 100, CurrencyCode: "JPY"}

	// 予算通知は1日に何度も届くので、2回目以降は通知しない。
	for i := 0; i < 3; i++ {
		if _, err := g.Handle(context.Background(), note); err != nil {
			t.Fatal(err)
		}
	}
	if !s.appStopped || s.billingOff {
		t.Fatalf("アプリだけを止める: %+v", s)
	}
	if len(n.sent) != 1 || !strings.Contains(n.sent[0], "60%") {
		t.Fatalf("通知は1回だけ: %v", n.sent)
	}
}

func TestHandleHardStop(t *testing.T) {
	s, n := &fakeStopper{}, &fakeNotifier{}
	if _, err := newGuard(s, n).Handle(context.Background(), Notification{CostAmount: 120, BudgetAmount: 100}); err != nil {
		t.Fatal(err)
	}
	if !s.appStopped || !s.billingOff || len(n.sent) != 1 {
		t.Fatalf("アプリを止めてから課金を外し、知らせる: %+v %v", s, n.sent)
	}
}

func TestHandleHardStopDisablesBillingEvenIfAppStopFails(t *testing.T) {
	s := &fakeStopper{appErr: errors.New("権限なし")}
	if _, err := newGuard(s, &fakeNotifier{}).Handle(context.Background(), Notification{CostAmount: 100, BudgetAmount: 100}); err != nil {
		t.Fatal(err)
	}
	if !s.billingOff {
		t.Fatal("アプリの停止に失敗しても課金は外す")
	}
}

func TestHandleReturnsErrorForRetry(t *testing.T) {
	s := &fakeStopper{billErr: errors.New("一時的なエラー")}
	if _, err := newGuard(s, nil).Handle(context.Background(), Notification{CostAmount: 100, BudgetAmount: 100}); err == nil {
		t.Fatal("課金の解除に失敗したらエラーを返し、Pub/Sub に再送させる")
	}
}
