package main

import (
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"photography-workflow/server/internal/payment"
)

type paymentOrder struct {
	ID                   uuid.UUID  `json:"id"`
	ProjectID            uuid.UUID  `json:"project_id"`
	ContractID           *uuid.UUID `json:"contract_id,omitempty"`
	Kind                 string     `json:"kind"`
	Status               string     `json:"status"`
	Currency             string     `json:"currency"`
	AmountCents          int64      `json:"amount_cents"`
	OutTradeNo           string     `json:"out_trade_no"`
	PayURL               string     `json:"pay_url"`
	ProjectPaymentStatus string     `json:"project_payment_status"`
}

type paymentRecord struct {
	ID          uuid.UUID `json:"id"`
	OrderID     uuid.UUID `json:"order_id"`
	AmountCents int64     `json:"amount_cents"`
	Currency    string    `json:"currency"`
}

type refundRecord struct {
	ID               uuid.UUID `json:"id"`
	PaymentID        uuid.UUID `json:"payment_id"`
	OutRefundNo      string    `json:"out_refund_no"`
	Status           string    `json:"status"`
	AmountCents      int64     `json:"amount_cents"`
	ProviderRefundID string    `json:"provider_refund_id"`
	FailureReason    string    `json:"failure_reason"`
}

type paymentList struct {
	Total int             `json:"total"`
	Items []paymentRecord `json:"items"`
}

type refundList struct {
	Total int            `json:"total"`
	Items []refundRecord `json:"items"`
}

// runPayments exercises the M2 loop: deposit order, idempotent reuse, verified
// callbacks with amount checks, refunds through the asynchronous callback,
// re-payment, booking and completion gates, and refund guards.
func runPayments(c *client, cfg smokeConfig, projectID uuid.UUID, tag string) error {
	step("M2 payment order does not require a signed contract")
	var unsigned struct {
		ID uuid.UUID `json:"id"`
	}
	if err := c.do(http.MethodPost, "/api/v1/projects", c.token,
		map[string]any{"title": "Unsigned " + tag}, &unsigned); err != nil {
		return err
	}
	var quote struct {
		ID         uuid.UUID `json:"id"`
		RowVersion int       `json:"row_version"`
	}
	if err := c.do(http.MethodPost, "/api/v1/quotes", c.token, map[string]any{
		"project_id":    unsigned.ID,
		"items":         []map[string]any{{"description": "Unsigned booking service", "quantity": 1, "unit_price_cents": 80000}},
		"deposit_cents": 20000,
	}, &quote); err != nil {
		return err
	}
	var sentQuote struct {
		ID uuid.UUID `json:"id"`
	}
	if err := c.do(http.MethodPost, "/api/v1/quotes/"+quote.ID.String()+"/send", c.token,
		map[string]any{"expected_version": quote.RowVersion}, &sentQuote); err != nil {
		return err
	}
	unsignedOrder, err := c.createOrder(unsigned.ID, "deposit")
	if err != nil {
		return fmt.Errorf("create payment order without a signed contract: %w", err)
	}
	if unsignedOrder.Status != "pending" || unsignedOrder.AmountCents != 20000 || unsignedOrder.ContractID != nil {
		return fmt.Errorf("unsigned project order = %+v, want pending 20000 with no contract", unsignedOrder)
	}
	unsignedPaidEvent := map[string]any{
		"event_id": "smoke-unsigned-deposit-" + tag, "type": payment.EventPaymentSucceeded,
		"merchant_id": cfg.PaymentMerchant, "out_trade_no": unsignedOrder.OutTradeNo,
		"provider_trade_no": "wx-unsigned-" + tag, "amount_cents": unsignedOrder.AmountCents,
		"currency": unsignedOrder.Currency, "paid_at": time.Now().UTC().Format(time.RFC3339),
	}
	if err := c.expectPaymentCallback(unsignedPaidEvent, cfg.PaymentSecret, 200, "processed"); err != nil {
		return fmt.Errorf("settle payment without a signed contract: %w", err)
	}
	var paidUnsigned paymentOrder
	if err := c.do(http.MethodGet, "/api/v1/payments/orders/"+unsignedOrder.ID.String(), c.token, nil, &paidUnsigned); err != nil {
		return err
	}
	if paidUnsigned.Status != "paid" || paidUnsigned.ProjectPaymentStatus != "deposit_paid" {
		return fmt.Errorf("unsigned project payment = %+v, want paid/deposit_paid", paidUnsigned)
	}
	fmt.Println("  unsigned project: sent quote -> 20000 deposit order -> payment settles")

	step("M2 deposit order")
	deposit, err := c.createOrder(projectID, "deposit")
	if err != nil {
		return err
	}
	if deposit.Status != "pending" || deposit.AmountCents != 50000 || deposit.Currency != "CNY" || deposit.PayURL == "" {
		return fmt.Errorf("deposit order = %+v", deposit)
	}
	if deposit.ProjectPaymentStatus != "deposit_pending" {
		return fmt.Errorf("project status after deposit order = %s, want deposit_pending", deposit.ProjectPaymentStatus)
	}
	reused, err := c.createOrder(projectID, "deposit")
	if err != nil {
		return err
	}
	if reused.ID != deposit.ID {
		return fmt.Errorf("duplicate deposit order created %s, want reuse of %s", reused.ID, deposit.ID)
	}
	project, err := c.getProject(projectID)
	if err != nil {
		return err
	}
	if project.PaymentStatus != "deposit_pending" {
		return fmt.Errorf("project payment_status = %s, want deposit_pending", project.PaymentStatus)
	}
	fmt.Printf("  order=%s amount=%d pending, duplicate create reuses the order\n", deposit.ID, deposit.AmountCents)

	step("M2 payment callbacks")
	wrongAmount := map[string]any{
		"event_id": "smoke-pay-wrong-" + tag, "type": payment.EventPaymentSucceeded,
		"merchant_id": cfg.PaymentMerchant, "out_trade_no": deposit.OutTradeNo,
		"provider_trade_no": "wx-wrong-" + tag, "amount_cents": 1, "currency": "CNY",
	}
	if err := c.expectPaymentCallback(wrongAmount, cfg.PaymentSecret, 422, "amount_mismatch"); err != nil {
		return err
	}
	payments, err := c.listPayments(projectID)
	if err != nil {
		return err
	}
	if payments.Total != 0 {
		return fmt.Errorf("amount mismatch credited %d payments", payments.Total)
	}
	project, err = c.getProject(projectID)
	if err != nil {
		return err
	}
	if project.PaymentStatus != "deposit_pending" {
		return fmt.Errorf("project status changed after a rejected callback")
	}
	tampered := map[string]any{
		"event_id": "smoke-pay-tampered-" + tag, "type": payment.EventPaymentSucceeded,
		"merchant_id": cfg.PaymentMerchant, "out_trade_no": deposit.OutTradeNo,
		"amount_cents": 50000, "currency": "CNY",
	}
	if err := c.expectPaymentCallback(tampered, cfg.PaymentSecret+"-wrong", 401, "invalid_signature"); err != nil {
		return err
	}
	// A signature-valid callback that names another merchant must not touch the
	// order, but has to surface as a critical security alert.
	foreignMerchant := map[string]any{
		"event_id": "smoke-pay-foreign-merchant-" + tag, "type": payment.EventPaymentSucceeded,
		"merchant_id": "someone-else", "out_trade_no": deposit.OutTradeNo,
		"amount_cents": 50000, "currency": "CNY",
	}
	if err := c.expectPaymentCallback(foreignMerchant, cfg.PaymentSecret, 401, "merchant_mismatch"); err != nil {
		return err
	}
	unknown := map[string]any{
		"event_id": "smoke-pay-unknown-" + tag, "type": payment.EventPaymentSucceeded,
		"merchant_id": cfg.PaymentMerchant, "out_trade_no": "PWFUNKNOWN" + tag,
		"amount_cents": 50000, "currency": "CNY",
	}
	if err := c.expectPaymentCallback(unknown, cfg.PaymentSecret, 200, "ignored"); err != nil {
		return err
	}

	paidEvent := map[string]any{
		"event_id": "smoke-pay-deposit-" + tag, "type": payment.EventPaymentSucceeded,
		"merchant_id": cfg.PaymentMerchant, "out_trade_no": deposit.OutTradeNo,
		"provider_trade_no": "wx-deposit-" + tag, "amount_cents": 50000, "currency": "CNY",
		"paid_at": time.Now().UTC().Format(time.RFC3339),
	}
	if err := c.expectPaymentCallback(paidEvent, cfg.PaymentSecret, 200, "processed"); err != nil {
		return err
	}
	project, err = c.getProject(projectID)
	if err != nil {
		return err
	}
	if project.PaymentStatus != "deposit_paid" {
		return fmt.Errorf("project payment_status = %s, want deposit_paid", project.PaymentStatus)
	}
	payments, err = c.listPayments(projectID)
	if err != nil {
		return err
	}
	if payments.Total != 1 {
		return fmt.Errorf("payments after deposit = %d, want 1", payments.Total)
	}
	if err := c.expectPaymentCallback(paidEvent, cfg.PaymentSecret, 200, "duplicate"); err != nil {
		return err
	}
	secondEvent := map[string]any{
		"event_id": "smoke-pay-deposit-again-" + tag, "type": payment.EventPaymentSucceeded,
		"merchant_id": cfg.PaymentMerchant, "out_trade_no": deposit.OutTradeNo,
		"provider_trade_no": "wx-deposit-" + tag, "amount_cents": 50000, "currency": "CNY",
	}
	if err := c.expectPaymentCallback(secondEvent, cfg.PaymentSecret, 200, "duplicate"); err != nil {
		return err
	}
	payments, err = c.listPayments(projectID)
	if err != nil {
		return err
	}
	if payments.Total != 1 {
		return fmt.Errorf("a second event credited the order twice: %d payments", payments.Total)
	}
	fmt.Println("  mismatch -> 422 and no credit, tampered -> 401, unknown -> ignored, duplicate events credit once")

	step("M2 refund")
	depositPayment := payments.Items[0]
	refund, err := c.createRefund(depositPayment.ID, 50000, "客户取消定金")
	if err != nil {
		return err
	}
	if refund.Status != "pending" || refund.ProviderRefundID == "" || refund.OutRefundNo == "" {
		return fmt.Errorf("refund = %+v", refund)
	}
	if err := c.expect(http.MethodPost, "/api/v1/payments/"+depositPayment.ID.String()+"/refunds",
		c.token, map[string]any{"reason": "double refund"}, 409, "refund_exceeds_remaining"); err != nil {
		return err
	}
	refundEvent := map[string]any{
		"event_id": "smoke-refund-deposit-" + tag, "type": payment.EventRefundSucceeded,
		"merchant_id": cfg.PaymentMerchant, "out_refund_no": refund.OutRefundNo,
		"provider_refund_id": refund.ProviderRefundID, "amount_cents": 50000, "currency": "CNY",
	}
	if err := c.expectPaymentCallback(refundEvent, cfg.PaymentSecret, 200, "processed"); err != nil {
		return err
	}
	refunds, err := c.listRefunds(depositPayment.ID)
	if err != nil {
		return err
	}
	if refunds.Total != 1 || refunds.Items[0].Status != "succeeded" {
		return fmt.Errorf("refunds after callback = %+v", refunds)
	}
	payments, err = c.listPayments(projectID)
	if err != nil {
		return err
	}
	if payments.Total != 1 {
		return fmt.Errorf("refund replaced the payment record: %d payments", payments.Total)
	}
	project, err = c.getProject(projectID)
	if err != nil {
		return err
	}
	if project.PaymentStatus != "unpaid" {
		return fmt.Errorf("project payment_status after refund = %s, want unpaid", project.PaymentStatus)
	}
	if err := c.expectPaymentCallback(refundEvent, cfg.PaymentSecret, 200, "duplicate"); err != nil {
		return err
	}
	fmt.Println("  refund pending -> succeeded callback, payment row kept, project back to unpaid")

	step("M2 re-pay deposit and book")
	deposit2, err := c.createOrder(projectID, "deposit")
	if err != nil {
		return err
	}
	if deposit2.ID == deposit.ID || deposit2.AmountCents != 50000 {
		return fmt.Errorf("re-payment order = %+v", deposit2)
	}
	deposit2Event := map[string]any{
		"event_id": "smoke-pay-deposit2-" + tag, "type": payment.EventPaymentSucceeded,
		"merchant_id": cfg.PaymentMerchant, "out_trade_no": deposit2.OutTradeNo,
		"provider_trade_no": "wx-deposit2-" + tag, "amount_cents": 50000, "currency": "CNY",
	}
	if err := c.expectPaymentCallback(deposit2Event, cfg.PaymentSecret, 200, "processed"); err != nil {
		return err
	}
	project, err = c.getProject(projectID)
	if err != nil {
		return err
	}
	if project.PaymentStatus != "deposit_paid" {
		return fmt.Errorf("project payment_status = %s, want deposit_paid", project.PaymentStatus)
	}

	// Refund failure drill on the second deposit: pending -> failed leaves the
	// projection and the payment untouched, and the amount becomes refundable
	// again (the success path is covered above).
	payments, err = c.listPayments(projectID)
	if err != nil {
		return err
	}
	deposit2Payment := payments.Items[0]
	failingRefund, err := c.createRefund(deposit2Payment.ID, 50000, "refund failure drill")
	if err != nil {
		return err
	}
	failedEvent := map[string]any{
		"event_id": "smoke-refund-failed-" + tag, "type": payment.EventRefundFailed,
		"merchant_id": cfg.PaymentMerchant, "out_refund_no": failingRefund.OutRefundNo,
		"amount_cents": 50000, "currency": "CNY", "reason": "insufficient merchant balance",
	}
	if err := c.expectPaymentCallback(failedEvent, cfg.PaymentSecret, 200, "processed"); err != nil {
		return err
	}
	failedRefunds, err := c.listRefunds(deposit2Payment.ID)
	if err != nil {
		return err
	}
	if failedRefunds.Total != 1 || failedRefunds.Items[0].Status != "failed" {
		return fmt.Errorf("failed refund = %+v", failedRefunds)
	}
	project, err = c.getProject(projectID)
	if err != nil {
		return err
	}
	if project.PaymentStatus != "deposit_paid" {
		return fmt.Errorf("failed refund changed payment_status")
	}

	version := project.Version
	var booked commerceProject
	if err := c.do(http.MethodPost, "/api/v1/projects/"+projectID.String()+"/transitions", c.token,
		map[string]any{"to": "booked", "expected_version": version}, &booked); err != nil {
		return err
	}
	version = booked.Version
	if booked.State != "booked" || booked.PaymentStatus != "deposit_paid" {
		return fmt.Errorf("project after booked = %+v", booked)
	}
	fmt.Println("  deposit re-paid, refund failure recorded, project -> booked")

	step("M2 balance, close, late payment and reconcile")
	balance, err := c.createOrder(projectID, "balance")
	if err != nil {
		return err
	}
	if balance.AmountCents != 200000 || balance.ProjectPaymentStatus != "balance_pending" {
		return fmt.Errorf("balance order = %+v", balance)
	}
	var closed paymentOrder
	if err := c.do(http.MethodPost, "/api/v1/payments/orders/"+balance.ID.String()+"/close", c.token, nil, &closed); err != nil {
		return err
	}
	if closed.Status != "closed" || closed.ProjectPaymentStatus != "deposit_paid" {
		return fmt.Errorf("closed order = %+v", closed)
	}

	// The customer paid just as the order was closed: the callback must be
	// flagged for manual resolution, never credited.
	lateEvent := map[string]any{
		"event_id": "smoke-pay-late-" + tag, "type": payment.EventPaymentSucceeded,
		"merchant_id": cfg.PaymentMerchant, "out_trade_no": balance.OutTradeNo,
		"provider_trade_no": "wx-balance-late-" + tag, "amount_cents": 200000, "currency": "CNY",
	}
	if err := c.expectPaymentCallback(lateEvent, cfg.PaymentSecret, 200, "ignored"); err != nil {
		return err
	}
	paymentsBefore, err := c.listPayments(projectID)
	if err != nil {
		return err
	}
	if err := c.expect(http.MethodPost, "/api/v1/payments/orders/"+balance.ID.String()+"/reconcile",
		c.token, nil, 409, "reconcile_mismatch"); err != nil {
		return err
	}
	payments, err = c.listPayments(projectID)
	if err != nil {
		return err
	}
	if payments.Total != paymentsBefore.Total {
		return fmt.Errorf("late payment credited a closed order: %d -> %d payments", paymentsBefore.Total, payments.Total)
	}

	balance2, err := c.createOrder(projectID, "balance")
	if err != nil {
		return err
	}
	if balance2.ID == balance.ID || balance2.ProjectPaymentStatus != "balance_pending" {
		return fmt.Errorf("balance order after close = %+v", balance2)
	}

	// A callback that is rejected on amount still leaves the channel paid.
	// Closing then must not abandon the captured funds: the service reconciles
	// the order as paid and answers 409 order_already_paid.
	badBalance := map[string]any{
		"event_id": "smoke-pay-balance-bad-" + tag, "type": payment.EventPaymentSucceeded,
		"merchant_id": cfg.PaymentMerchant, "out_trade_no": balance2.OutTradeNo,
		"provider_trade_no": "wx-balance-bad-" + tag, "amount_cents": 1, "currency": "CNY",
	}
	if err := c.expectPaymentCallback(badBalance, cfg.PaymentSecret, 422, "amount_mismatch"); err != nil {
		return err
	}
	if err := c.expect(http.MethodPost, "/api/v1/payments/orders/"+balance2.ID.String()+"/close",
		c.token, nil, 409, "order_already_paid"); err != nil {
		return err
	}
	reconciled, err := c.reconcileOrder(balance2.ID)
	if err != nil {
		return err
	}
	if reconciled.Status != "paid" || reconciled.ProjectPaymentStatus != "paid" {
		return fmt.Errorf("reconciled order = %+v", reconciled)
	}
	if _, err := c.reconcileOrder(balance2.ID); err != nil {
		return err
	}
	payments, err = c.listPayments(projectID)
	if err != nil {
		return err
	}
	if payments.Total != paymentsBefore.Total+1 {
		return fmt.Errorf("reconcile credit count = %d, want %d", payments.Total, paymentsBefore.Total+1)
	}
	project, err = c.getProject(projectID)
	if err != nil {
		return err
	}
	if project.PaymentStatus != "paid" {
		return fmt.Errorf("project payment_status = %s, want paid", project.PaymentStatus)
	}
	fmt.Println("  closed order late payment -> ignored, reconcile mismatch -> 409, close after channel payment -> 409 order_already_paid, reconcile credits once")
	if err := c.expect(http.MethodPost, "/api/v1/payments/orders", c.token,
		map[string]any{"project_id": projectID, "kind": "balance"}, 409, "already_paid"); err != nil {
		return err
	}
	var lastProject commerceProject
	for _, to := range []string{"shooting", "post_production", "completed"} {
		if err := c.do(http.MethodPost, "/api/v1/projects/"+projectID.String()+"/transitions", c.token,
			map[string]any{"to": to, "expected_version": version}, &lastProject); err != nil {
			return err
		}
		version = lastProject.Version
	}
	if lastProject.State != "completed" || lastProject.PaymentStatus != "paid" {
		return fmt.Errorf("project after completion = %+v", lastProject)
	}
	fmt.Println("  balance closed/reopened, paid, project -> completed with payment_status=paid")

	step("M2 refund guard")
	if err := c.expect(http.MethodPost, "/api/v1/payments/"+depositPayment.ID.String()+"/refunds",
		c.token, map[string]any{"reason": "already refunded"}, 409, "refund_exceeds_remaining"); err != nil {
		return err
	}
	fmt.Println("  fully refunded payment rejects another refund with 409")
	return runInvoices(c, cfg, projectID, tag, payments.Items)
}

func (c *client) createOrder(projectID uuid.UUID, kind string) (*paymentOrder, error) {
	var order paymentOrder
	if err := c.do(http.MethodPost, "/api/v1/payments/orders", c.token,
		map[string]any{"project_id": projectID, "kind": kind}, &order); err != nil {
		return nil, err
	}
	return &order, nil
}

func (c *client) reconcileOrder(orderID uuid.UUID) (*paymentOrder, error) {
	var order paymentOrder
	if err := c.do(http.MethodPost, "/api/v1/payments/orders/"+orderID.String()+"/reconcile",
		c.token, nil, &order); err != nil {
		return nil, err
	}
	return &order, nil
}

func (c *client) createRefund(paymentID uuid.UUID, amount int64, reason string) (*refundRecord, error) {
	var refund refundRecord
	if err := c.do(http.MethodPost, "/api/v1/payments/"+paymentID.String()+"/refunds", c.token,
		map[string]any{"amount_cents": amount, "reason": reason}, &refund); err != nil {
		return nil, err
	}
	return &refund, nil
}

func (c *client) getProject(projectID uuid.UUID) (*commerceProject, error) {
	var p commerceProject
	if err := c.do(http.MethodGet, "/api/v1/projects/"+projectID.String(), c.token, nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (c *client) listPayments(projectID uuid.UUID) (*paymentList, error) {
	var list paymentList
	if err := c.do(http.MethodGet, "/api/v1/payments?project_id="+projectID.String(), c.token, nil, &list); err != nil {
		return nil, err
	}
	return &list, nil
}

func (c *client) listRefunds(paymentID uuid.UUID) (*refundList, error) {
	var list refundList
	if err := c.do(http.MethodGet, "/api/v1/payments/"+paymentID.String()+"/refunds", c.token, nil, &list); err != nil {
		return nil, err
	}
	return &list, nil
}

func (c *client) expectPaymentCallback(body map[string]any, secret string, wantStatus int, wantResult string) error {
	return c.expectWebhook("/webhooks/payment/sandbox", payment.SignRequestBody, body, secret, wantStatus, wantResult)
}
