package changeorder

import (
	"errors"
	"strings"
	"testing"
)

func TestValidStatus(t *testing.T) {
	for _, s := range []string{StatusDraft, StatusAwaitingSignature, StatusSigned,
		StatusAwaitingPayment, StatusApplied, StatusCancelled, StatusReversed} {
		if !ValidStatus(s) {
			t.Fatalf("%s should be valid", s)
		}
	}
	if ValidStatus("paid") || ValidStatus("") {
		t.Fatal("unknown status accepted")
	}
}

func TestValidItemKind(t *testing.T) {
	for _, k := range []string{ItemExtraRetouch, ItemReshoot, ItemTravel, ItemRush, ItemOther} {
		if !ValidItemKind(k) {
			t.Fatalf("%s should be valid", k)
		}
	}
	if ValidItemKind("discount") || ValidItemKind("") {
		t.Fatal("unknown item kind accepted")
	}
}

func TestStatusTransitions(t *testing.T) {
	cases := []struct {
		name                  string
		status                string
		send, cancel, collect bool
	}{
		{"draft", StatusDraft, true, true, false},
		{"awaiting signature", StatusAwaitingSignature, false, true, false},
		{"signed", StatusSigned, false, true, true},
		{"awaiting payment", StatusAwaitingPayment, false, true, true},
		{"applied", StatusApplied, false, false, false},
		{"cancelled", StatusCancelled, false, false, false},
		{"reversed", StatusReversed, false, false, false},
	}
	for _, tc := range cases {
		if got := CanSend(tc.status); got != tc.send {
			t.Errorf("%s: CanSend = %v, want %v", tc.name, got, tc.send)
		}
		if got := CanCancel(tc.status); got != tc.cancel {
			t.Errorf("%s: CanCancel = %v, want %v", tc.name, got, tc.cancel)
		}
		if got := CanCollect(tc.status); got != tc.collect {
			t.Errorf("%s: CanCollect = %v, want %v", tc.name, got, tc.collect)
		}
	}
	if !CanReverse(StatusApplied) || CanReverse(StatusCancelled) {
		t.Fatal("only applied amendments can reverse")
	}
}

func TestPrepareItems(t *testing.T) {
	items, total, err := prepareItems([]ItemInput{
		{Kind: ItemReshoot, Description: "补拍婚礼外景", Quantity: 2, UnitPriceCents: 20000},
		{Description: "加急精修", Quantity: 1, UnitPriceCents: 30000},
	})
	if err != nil {
		t.Fatalf("prepareItems: %v", err)
	}
	if total != 70000 {
		t.Fatalf("total = %d, want 70000", total)
	}
	if items[1].Kind != ItemOther {
		t.Fatalf("empty kind should default to other, got %s", items[1].Kind)
	}
	if items[0].AmountCents != 40000 || items[1].AmountCents != 30000 {
		t.Fatalf("line amounts = %+v", items)
	}

	bad := []struct {
		name  string
		items []ItemInput
	}{
		{"no items", nil},
		{"unknown kind", []ItemInput{{Kind: "discount", Description: "x", Quantity: 1, UnitPriceCents: 100}}},
		{"missing description", []ItemInput{{Quantity: 1, UnitPriceCents: 100}}},
		{"long description", []ItemInput{{Description: strings.Repeat("字", 301), Quantity: 1, UnitPriceCents: 100}}},
		{"zero quantity", []ItemInput{{Description: "x", Quantity: 0, UnitPriceCents: 100}}},
		{"negative price", []ItemInput{{Description: "x", Quantity: 1, UnitPriceCents: -1}}},
		{"zero amount", []ItemInput{{Description: "x", Quantity: 1, UnitPriceCents: 0}}},
	}
	for _, tc := range bad {
		if _, _, err := prepareItems(tc.items); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: err = %v, want ErrInvalidInput", tc.name, err)
		}
	}

	tooMany := make([]ItemInput, 51)
	for i := range tooMany {
		tooMany[i] = ItemInput{Description: "x", Quantity: 1, UnitPriceCents: 100}
	}
	if _, _, err := prepareItems(tooMany); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("51 items: err = %v, want ErrInvalidInput", err)
	}
}
