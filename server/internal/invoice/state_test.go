package invoice

import (
	"context"
	"testing"
)

func TestStateMachine(t *testing.T) {
	cases := []struct {
		status  string
		approve bool
		reject  bool
		issue   bool
		send    bool
		void    bool
	}{
		{StatusRequested, true, true, false, false, true},
		{StatusApproved, false, false, true, false, true},
		{StatusRejected, false, false, false, false, false},
		{StatusIssued, false, false, false, true, false},
		{StatusSent, false, false, false, false, false},
		{StatusVoided, false, false, false, false, false},
	}
	for _, tc := range cases {
		if got := CanApprove(tc.status); got != tc.approve {
			t.Errorf("CanApprove(%s) = %v, want %v", tc.status, got, tc.approve)
		}
		if got := CanReject(tc.status); got != tc.reject {
			t.Errorf("CanReject(%s) = %v, want %v", tc.status, got, tc.reject)
		}
		if got := CanIssue(tc.status); got != tc.issue {
			t.Errorf("CanIssue(%s) = %v, want %v", tc.status, got, tc.issue)
		}
		if got := CanSend(tc.status); got != tc.send {
			t.Errorf("CanSend(%s) = %v, want %v", tc.status, got, tc.send)
		}
		if got := CanVoid(tc.status); got != tc.void {
			t.Errorf("CanVoid(%s) = %v, want %v", tc.status, got, tc.void)
		}
	}
	if !ValidType(TypeNormal) || !ValidType(TypeSpecial) || ValidType("dedicated") {
		t.Fatal("ValidType must accept normal/special only")
	}
}

func TestManualProvider(t *testing.T) {
	provider, err := NewProvider("")
	if err != nil || provider.Name() != "manual" {
		t.Fatalf("default provider = %v, %v", provider, err)
	}
	if _, err := NewProvider("fapiao-cloud"); err == nil {
		t.Fatal("unknown provider must be rejected at startup")
	}
	res, err := provider.Issue(context.Background(), IssueRequest{
		InvoiceNo: " 12345678 ", InvoiceCode: " 044031900111 ",
	})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if res.InvoiceNo != "12345678" || res.InvoiceCode != "044031900111" {
		t.Fatalf("issue result = %+v", res)
	}
	if _, err := provider.Issue(context.Background(), IssueRequest{}); err == nil {
		t.Fatal("empty invoice number must be rejected")
	}
	rf, err := provider.RedFlush(context.Background(), RedFlushRequest{RedFlushNo: "87654321"})
	if err != nil || rf.RedFlushNo != "87654321" {
		t.Fatalf("red flush = %+v, %v", rf, err)
	}
	if _, err := provider.RedFlush(context.Background(), RedFlushRequest{}); err == nil {
		t.Fatal("empty red flush number must be rejected")
	}
}
