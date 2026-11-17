package payment

import (
	"testing"
	"time"
)

func TestValidKind(t *testing.T) {
	for _, kind := range []string{KindDeposit, KindBalance, KindFull, KindChange} {
		if !ValidKind(kind) {
			t.Fatalf("%s should be valid", kind)
		}
	}
	if ValidKind("retainer") || ValidKind("") {
		t.Fatal("unknown kind accepted")
	}
}

func TestReusableOrder(t *testing.T) {
	now := time.Now()
	o := &Order{Status: OrderPending, AmountCents: 50000, Currency: "CNY", PayURL: "https://pay/x", ExpiresAt: now.Add(time.Minute)}
	if !reusableOrder(o, 50000, "CNY") {
		t.Fatal("a live order with a matching amount should be reusable")
	}
	if reusableOrder(o, 50001, "CNY") || reusableOrder(o, 50000, "USD") {
		t.Fatal("amount or currency change must not reuse the order")
	}

	o.ExpiresAt = now.Add(-time.Minute)
	if reusableOrder(o, 50000, "CNY") {
		t.Fatal("an expired order must not be reused")
	}
	o.ExpiresAt = now.Add(time.Minute)

	o.Status = OrderCreating
	if reusableOrder(o, 50000, "CNY") {
		t.Fatal("a creating reservation must not be reused")
	}

	o.Status = OrderPending
	o.PayURL = ""
	if reusableOrder(o, 50000, "CNY") {
		t.Fatal("an order without a pay url must not be reused")
	}
}

func TestDeriveProjectStatus(t *testing.T) {
	const (
		total   = int64(250000)
		deposit = int64(50000)
	)
	cases := []struct {
		name                                    string
		total, effective, deposit, net, coreNet int64
		pendingDeposit                          bool
		pendingFull                             bool
		pendingBalance                          bool
		want                                    string
	}{
		{"nothing", total, total, deposit, 0, 0, false, false, false, StatusUnpaid},
		{"deposit order open", total, total, deposit, 0, 0, true, false, false, StatusDepositPending},
		{"deposit paid", total, total, deposit, 50000, 50000, false, false, false, StatusDepositPaid},
		{"balance order open", total, total, deposit, 50000, 50000, false, false, true, StatusBalancePending},
		{"fully paid", total, total, deposit, 250000, 250000, false, false, true, StatusPaid},
		{"overpaid", total, total, deposit, 300000, 300000, false, false, false, StatusPaid},
		{"refunded below deposit", total, total, deposit, 20000, 20000, false, false, false, StatusUnpaid},
		{"refunded to zero", total, total, deposit, -5, -5, false, false, false, StatusUnpaid},
		{"full payment open", 100000, 100000, 0, 0, 0, false, true, false, StatusDepositPending},
		{"full payment paid", 100000, 100000, 0, 100000, 100000, false, false, false, StatusPaid},
		{"no total", 0, 0, 0, 0, 0, false, false, false, StatusUnpaid},
		{"change applied unpaid", total, 300000, deposit, 250000, 250000, false, false, false, StatusChangePending},
		{"change paid", total, 300000, deposit, 300000, 250000, false, false, false, StatusPaid},
		{"change applied before core settled", total, 300000, deposit, 150000, 50000, false, false, false, StatusDepositPaid},
		{"change reversed", total, total, deposit, 250000, 250000, false, false, false, StatusPaid},
	}
	for _, tc := range cases {
		got := DeriveProjectStatus(tc.total, tc.effective, tc.deposit, tc.net, tc.coreNet,
			tc.pendingDeposit, tc.pendingFull, tc.pendingBalance)
		if got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
}
