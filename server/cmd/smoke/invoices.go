package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"photography-workflow/server/internal/payment"
)

type invoiceRecord struct {
	ID          uuid.UUID        `json:"id"`
	ProjectID   uuid.UUID        `json:"project_id"`
	PaymentID   uuid.UUID        `json:"payment_id"`
	Type        string           `json:"type"`
	Status      string           `json:"status"`
	Title       string           `json:"title"`
	Email       string           `json:"email"`
	AmountCents int64            `json:"amount_cents"`
	Currency    string           `json:"currency"`
	InvoiceNo   string           `json:"invoice_no"`
	InvoiceCode string           `json:"invoice_code"`
	FileKey     string           `json:"file_key"`
	FileHash    string           `json:"file_hash"`
	FileSize    int64            `json:"file_size"`
	Version     int              `json:"version"`
	RedFlushes  []redFlushRecord `json:"red_flushes"`
}

type redFlushRecord struct {
	ID               uuid.UUID `json:"id"`
	InvoiceID        uuid.UUID `json:"invoice_id"`
	RefundID         uuid.UUID `json:"refund_id"`
	AmountCents      int64     `json:"amount_cents"`
	Status           string    `json:"status"`
	RedFlushNo       string    `json:"red_flush_no"`
	ProviderFileHash string    `json:"file_hash"`
}

type invoiceList struct {
	Total int             `json:"total"`
	Items []invoiceRecord `json:"items"`
}

type invoiceFileUpload struct {
	ObjectKey string    `json:"object_key"`
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// runInvoices exercises the manual invoicing loop: request, approve, upload the
// verified PDF, issue, send, then a partial refund that must queue a red flush
// which itself requires a verified file. Duplicate requests and repeated
// transitions keep the same idempotency/conflict contract as payments.
func runInvoices(c *client, cfg smokeConfig, projectID uuid.UUID, tag string, payments []paymentRecord) error {
	balancePayment, ok := paymentByAmount(payments, 200000)
	if !ok {
		return fmt.Errorf("no 200000 payment found for invoice smoke: %+v", payments)
	}
	depositPayment, ok := paymentByAmount(payments, 50000)
	if !ok {
		return fmt.Errorf("no 50000 payment found for invoice smoke: %+v", payments)
	}

	step("M2 invoice request")
	var inv invoiceRecord
	if err := c.do(http.MethodPost, "/api/v1/invoices", c.token, map[string]any{
		"payment_id": balancePayment.ID, "type": "normal", "title": "Smoke Media " + tag,
		"tax_no": "91310000MA1FL0SMOKE", "email": "billing@smoke.example",
		"address": "上海市徐汇区", "phone": "13800000000", "remark": "smoke invoice",
	}, &inv); err != nil {
		return err
	}
	if inv.Status != "requested" || inv.Version != 1 {
		return fmt.Errorf("invoice request = %+v", inv)
	}
	if inv.AmountCents != balancePayment.AmountCents || inv.Currency != balancePayment.Currency {
		return fmt.Errorf("invoice amount = %d %s, want the payment amount %d %s",
			inv.AmountCents, inv.Currency, balancePayment.AmountCents, balancePayment.Currency)
	}
	if err := c.expect(http.MethodPost, "/api/v1/invoices", c.token, map[string]any{
		"payment_id": balancePayment.ID, "title": "Duplicate",
	}, 409, "active_invoice"); err != nil {
		return err
	}
	if err := c.expect(http.MethodPost, "/api/v1/invoices/"+inv.ID.String()+"/send", c.token,
		map[string]any{"expected_version": inv.Version}, 409, "illegal_state"); err != nil {
		return err
	}
	if err := c.expect(http.MethodPost, "/api/v1/invoices/"+inv.ID.String()+"/file-url", c.token,
		nil, 409, "illegal_state"); err != nil {
		return err
	}
	fmt.Println("  requested amount mirrors the payment, duplicate -> 409 active_invoice, early send/file -> 409 illegal_state")

	step("M2 invoice approve and issue")
	var approved invoiceRecord
	if err := c.do(http.MethodPost, "/api/v1/invoices/"+inv.ID.String()+"/approve", c.token,
		map[string]any{"expected_version": inv.Version}, &approved); err != nil {
		return err
	}
	if approved.Status != "approved" || approved.Version != inv.Version+1 {
		return fmt.Errorf("approved invoice = %+v", approved)
	}
	content := []byte("%PDF-1.4\n% smoke invoice " + tag + "\n%%EOF\n")
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])
	upload, err := c.invoiceFileURL(inv.ID, nil)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(upload.ObjectKey, "tenants/") {
		return fmt.Errorf("invoice object key = %s", upload.ObjectKey)
	}
	if err := c.putFile(upload.URL, content); err != nil {
		return err
	}
	if err := c.expect(http.MethodPost, "/api/v1/invoices/"+inv.ID.String()+"/issue", c.token, map[string]any{
		"expected_version": approved.Version, "invoice_no": "INV-" + tag, "invoice_code": "044031900111",
		"file_key": upload.ObjectKey, "file_hash": strings.Repeat("0", 64),
	}, 422, "validation_failed"); err != nil {
		return err
	}
	var issued invoiceRecord
	if err := c.do(http.MethodPost, "/api/v1/invoices/"+inv.ID.String()+"/issue", c.token, map[string]any{
		"expected_version": approved.Version, "invoice_no": "INV-" + tag, "invoice_code": "044031900111",
		"file_key": upload.ObjectKey, "file_hash": hash,
	}, &issued); err != nil {
		return err
	}
	if issued.Status != "issued" || issued.InvoiceNo != "INV-"+tag || issued.FileHash != hash || issued.FileSize != int64(len(content)) {
		return fmt.Errorf("issued invoice = %+v", issued)
	}
	if issued.Version != approved.Version+1 {
		return fmt.Errorf("issued version = %d, want %d", issued.Version, approved.Version+1)
	}
	if err := c.expect(http.MethodPost, "/api/v1/invoices/"+inv.ID.String()+"/issue", c.token, map[string]any{
		"expected_version": issued.Version, "invoice_no": "INV-" + tag + "-again",
		"file_key": upload.ObjectKey, "file_hash": hash,
	}, 409, "illegal_state"); err != nil {
		return err
	}
	var sent invoiceRecord
	if err := c.do(http.MethodPost, "/api/v1/invoices/"+inv.ID.String()+"/send", c.token,
		map[string]any{"expected_version": issued.Version}, &sent); err != nil {
		return err
	}
	if sent.Status != "sent" {
		return fmt.Errorf("sent invoice = %+v", sent)
	}
	fmt.Printf("  approved -> issued (hash verified) -> sent, duplicate issue -> 409 illegal_state\n")

	step("M2 invoice number uniqueness")
	var second invoiceRecord
	if err := c.do(http.MethodPost, "/api/v1/invoices", c.token, map[string]any{
		"payment_id": depositPayment.ID, "title": "Smoke Second " + tag, "email": "billing@smoke.example",
	}, &second); err != nil {
		return err
	}
	var secondApproved invoiceRecord
	if err := c.do(http.MethodPost, "/api/v1/invoices/"+second.ID.String()+"/approve", c.token,
		map[string]any{"expected_version": second.Version}, &secondApproved); err != nil {
		return err
	}
	secondUpload, err := c.invoiceFileURL(second.ID, nil)
	if err != nil {
		return err
	}
	secondContent := []byte("%PDF-1.4\n% smoke second " + tag + "\n%%EOF\n")
	secondSum := sha256.Sum256(secondContent)
	if err := c.putFile(secondUpload.URL, secondContent); err != nil {
		return err
	}
	if err := c.expect(http.MethodPost, "/api/v1/invoices/"+second.ID.String()+"/issue", c.token, map[string]any{
		"expected_version": secondApproved.Version, "invoice_no": "INV-" + tag,
		"file_key": secondUpload.ObjectKey, "file_hash": hex.EncodeToString(secondSum[:]),
	}, 409, "invoice_no_conflict"); err != nil {
		return err
	}
	var voided invoiceRecord
	if err := c.do(http.MethodPost, "/api/v1/invoices/"+second.ID.String()+"/void", c.token,
		map[string]any{"expected_version": secondApproved.Version, "reason": "smoke conflict drill"}, &voided); err != nil {
		return err
	}
	if voided.Status != "voided" {
		return fmt.Errorf("voided invoice = %+v", voided)
	}
	fmt.Println("  reusing an invoice number -> 409 invoice_no_conflict, approved invoice voided")

	step("M2 invoice red flush")
	refund, err := c.createRefund(balancePayment.ID, 50000, "invoice partial refund")
	if err != nil {
		return err
	}
	refundEvent := map[string]any{
		"event_id": "smoke-invoice-refund-" + tag, "type": payment.EventRefundSucceeded,
		"merchant_id": cfg.PaymentMerchant, "out_refund_no": refund.OutRefundNo,
		"provider_refund_id": refund.ProviderRefundID, "amount_cents": 50000, "currency": "CNY",
	}
	if err := c.expectPaymentCallback(refundEvent, cfg.PaymentSecret, 200, "processed"); err != nil {
		return err
	}
	var afterRefund invoiceRecord
	if err := c.do(http.MethodGet, "/api/v1/invoices/"+inv.ID.String(), c.token, nil, &afterRefund); err != nil {
		return err
	}
	if len(afterRefund.RedFlushes) != 1 {
		return fmt.Errorf("red flushes after refund = %+v", afterRefund.RedFlushes)
	}
	redFlush := afterRefund.RedFlushes[0]
	if redFlush.Status != "pending" || redFlush.AmountCents != 50000 || redFlush.RefundID != refund.ID {
		return fmt.Errorf("queued red flush = %+v", redFlush)
	}
	if err := c.expectPaymentCallback(refundEvent, cfg.PaymentSecret, 200, "duplicate"); err != nil {
		return err
	}
	var duplicates invoiceRecord
	if err := c.do(http.MethodGet, "/api/v1/invoices/"+inv.ID.String(), c.token, nil, &duplicates); err != nil {
		return err
	}
	if len(duplicates.RedFlushes) != 1 {
		return fmt.Errorf("duplicate refund callback queued %d red flushes", len(duplicates.RedFlushes))
	}

	redContent := []byte("%PDF-1.4\n% smoke red flush " + tag + "\n%%EOF\n")
	redSum := sha256.Sum256(redContent)
	redUpload, err := c.invoiceFileURL(inv.ID, &redFlush.ID)
	if err != nil {
		return err
	}
	if err := c.putFile(redUpload.URL, redContent); err != nil {
		return err
	}
	var issuedRed redFlushRecord
	if err := c.do(http.MethodPost, "/api/v1/invoices/"+inv.ID.String()+"/red-flushes/"+redFlush.ID.String()+"/issue",
		c.token, map[string]any{
			"red_flush_no": "RF-" + tag, "red_flush_code": "044031900112",
			"file_key": redUpload.ObjectKey, "file_hash": hex.EncodeToString(redSum[:]),
		}, &issuedRed); err != nil {
		return err
	}
	if issuedRed.Status != "issued" || issuedRed.RedFlushNo != "RF-"+tag {
		return fmt.Errorf("issued red flush = %+v", issuedRed)
	}
	if err := c.expect(http.MethodPost, "/api/v1/invoices/"+inv.ID.String()+"/red-flushes/"+redFlush.ID.String()+"/issue",
		c.token, map[string]any{"red_flush_no": "RF-" + tag}, 409, "illegal_state"); err != nil {
		return err
	}
	if err := c.expect(http.MethodPost, "/api/v1/invoices/"+inv.ID.String()+"/void", c.token,
		map[string]any{"expected_version": afterRefund.Version, "reason": "issued"}, 409, "illegal_state"); err != nil {
		return err
	}
	var listed invoiceList
	if err := c.do(http.MethodGet, "/api/v1/invoices?project_id="+projectID.String()+"&limit=100", c.token, nil, &listed); err != nil {
		return err
	}
	if listed.Total < 2 {
		return fmt.Errorf("invoice list total = %d, want at least 2", listed.Total)
	}
	fmt.Println("  refund -> pending red flush (duplicate callback no-op) -> verified red flush issue; issued invoice cannot be voided")
	return runAlerts(c, cfg, projectID, tag)
}

func (c *client) invoiceFileURL(invoiceID uuid.UUID, redFlushID *uuid.UUID) (*invoiceFileUpload, error) {
	body := map[string]any{}
	if redFlushID != nil {
		body["red_flush_id"] = *redFlushID
	}
	var upload invoiceFileUpload
	if err := c.do(http.MethodPost, "/api/v1/invoices/"+invoiceID.String()+"/file-url", c.token, body, &upload); err != nil {
		return nil, err
	}
	if upload.ObjectKey == "" || upload.URL == "" {
		return nil, fmt.Errorf("file-url returned an empty upload: %+v", upload)
	}
	return &upload, nil
}

func paymentByAmount(items []paymentRecord, amount int64) (paymentRecord, bool) {
	for _, p := range items {
		if p.AmountCents == amount {
			return p, true
		}
	}
	return paymentRecord{}, false
}
