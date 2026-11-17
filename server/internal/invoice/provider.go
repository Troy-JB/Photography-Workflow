package invoice

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Provider abstracts the invoicing vendor. M2 ships the manual adapter: the
// operator issues the invoice in the tax system and records the numbers here,
// so "Issue" validates and echoes the recorded identity. A real vendor adapter
// (e-invoice API) implements the same interface without touching the workflow.
type Provider interface {
	Name() string
	Issue(ctx context.Context, req IssueRequest) (*IssueResult, error)
	RedFlush(ctx context.Context, req RedFlushRequest) (*RedFlushResult, error)
}

type IssueRequest struct {
	InvoiceID   uuid.UUID
	InvoiceNo   string
	InvoiceCode string
	Title       string
	TaxNo       string
	AmountCents int64
	Currency    string
}

type IssueResult struct {
	InvoiceNo   string
	InvoiceCode string
}

type RedFlushRequest struct {
	InvoiceID    uuid.UUID
	RedFlushID   uuid.UUID
	RedFlushNo   string
	RedFlushCode string
	AmountCents  int64
	Currency     string
	Reason       string
}

type RedFlushResult struct {
	RedFlushNo   string
	RedFlushCode string
}

func NewProvider(name string) (Provider, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "manual":
		return manualProvider{}, nil
	default:
		return nil, fmt.Errorf("invoice provider %q is not supported (supported: manual)", name)
	}
}

type manualProvider struct{}

func (manualProvider) Name() string { return "manual" }

func (manualProvider) Issue(_ context.Context, req IssueRequest) (*IssueResult, error) {
	no := strings.TrimSpace(req.InvoiceNo)
	code := strings.TrimSpace(req.InvoiceCode)
	if no == "" || len(no) > 40 || len(code) > 40 {
		return nil, fmt.Errorf("invoice number is required and must be at most 40 characters")
	}
	return &IssueResult{InvoiceNo: no, InvoiceCode: code}, nil
}

func (manualProvider) RedFlush(_ context.Context, req RedFlushRequest) (*RedFlushResult, error) {
	no := strings.TrimSpace(req.RedFlushNo)
	code := strings.TrimSpace(req.RedFlushCode)
	if no == "" || len(no) > 40 || len(code) > 40 {
		return nil, fmt.Errorf("red flush number is required and must be at most 40 characters")
	}
	return &RedFlushResult{RedFlushNo: no, RedFlushCode: code}, nil
}
