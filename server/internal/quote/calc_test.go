package quote

import (
	"errors"
	"math"
	"testing"
)

func TestCalculate(t *testing.T) {
	subtotal, total, err := Calculate([]ItemInput{
		{Kind: "service", Description: "拍摄", Quantity: 2, UnitPriceCents: 10_000},
		{Kind: "retouch", Description: "精修", Quantity: 1, UnitPriceCents: 5_000},
	}, 2_000, 1_000)
	if err != nil {
		t.Fatalf("calculate: %v", err)
	}
	if subtotal != 25_000 || total != 24_000 {
		t.Fatalf("subtotal/total = %d/%d", subtotal, total)
	}
}

func TestCalculateDefaultsKind(t *testing.T) {
	items := []ItemInput{{Description: "场地", Quantity: 1, UnitPriceCents: 100}}
	if _, _, err := Calculate(items, 0, 0); err != nil {
		t.Fatalf("calculate: %v", err)
	}
	if items[0].Kind != "service" {
		t.Fatalf("kind = %q, want service", items[0].Kind)
	}
}

func TestCalculateRejectsInvalidInput(t *testing.T) {
	valid := []ItemInput{{Description: "拍摄", Quantity: 1, UnitPriceCents: 100}}
	cases := []struct {
		name     string
		items    []ItemInput
		discount int64
		tax      int64
	}{
		{"no items", nil, 0, 0},
		{"bad kind", []ItemInput{{Kind: "gift", Description: "x", Quantity: 1}}, 0, 0},
		{"no description", []ItemInput{{Quantity: 1}}, 0, 0},
		{"zero quantity", []ItemInput{{Description: "x", Quantity: 0}}, 0, 0},
		{"negative price", []ItemInput{{Description: "x", Quantity: 1, UnitPriceCents: -1}}, 0, 0},
		{"negative discount", valid, -1, 0},
		{"discount exceeds subtotal", valid, 101, 0},
		{"item overflow", []ItemInput{{Description: "x", Quantity: 2, UnitPriceCents: math.MaxInt64}}, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := Calculate(c.items, c.discount, c.tax); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("err = %v, want invalid input", err)
			}
		})
	}
}

func TestStatePredicates(t *testing.T) {
	if !Editable(StatusDraft) || Editable(StatusSent) {
		t.Fatal("only drafts are editable")
	}
	if !CanSend(StatusDraft) || CanSend(StatusAccepted) {
		t.Fatal("only drafts can be sent")
	}
	if !CanAccept(StatusSent) || !CanReject(StatusSent) || CanAccept(StatusDraft) {
		t.Fatal("only sent quotes can be decided")
	}
	for _, s := range []string{StatusDraft, StatusSent, StatusAccepted, StatusRejected, StatusExpired, StatusSuperseded} {
		if !ValidStatus(s) {
			t.Fatalf("%s should be valid", s)
		}
	}
	if ValidStatus("unknown") {
		t.Fatal("unknown status accepted")
	}
}
