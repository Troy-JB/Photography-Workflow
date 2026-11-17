package main

import (
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"photography-workflow/server/internal/payment"
)

type changeOrder struct {
	ID                 uuid.UUID         `json:"id"`
	ProjectID          uuid.UUID         `json:"project_id"`
	OrderNo            string            `json:"order_no"`
	Status             string            `json:"status"`
	Currency           string            `json:"currency"`
	AmountCents        int64             `json:"amount_cents"`
	SignTaskID         string            `json:"sign_task_id"`
	SignURL            string            `json:"sign_url"`
	SignerName         string            `json:"signer_name"`
	SignedDocumentHash string            `json:"signed_document_hash"`
	SignedAt           *time.Time        `json:"signed_at"`
	PaymentOrderID     *uuid.UUID        `json:"payment_order_id"`
	AppliedAt          *time.Time        `json:"applied_at"`
	VoidReason         string            `json:"void_reason"`
	Version            int               `json:"version"`
	Items              []changeOrderItem `json:"items"`
}

type changeOrderItem struct {
	Kind        string `json:"kind"`
	Description string `json:"description"`
	Quantity    int    `json:"quantity"`
	AmountCents int64  `json:"amount_cents"`
}

type changeOrderList struct {
	Total int           `json:"total"`
	Items []changeOrder `json:"items"`
}

// runChangeOrders exercises the post-signing amendment loop: create with line
// items, hand it to the e-sign provider, receive the signed callback that opens
// the supplement payment, credit it, then refund (partial alert + full
// reversal), and finally the cancel path that closes a live supplement order.
func runChangeOrders(c *client, cfg smokeConfig, projectID uuid.UUID, tag string) error {
	step("M2 change order create")
	body := map[string]any{
		"project_id": projectID, "reason": "新增外景补拍与加急精修",
		"signer_name": "Smoke Client " + tag, "signer_contact": "13800000000",
		"items": []map[string]any{
			{"kind": "reshoot", "description": "婚礼外景补拍", "quantity": 1, "unit_price_cents": 50000},
			{"kind": "extra_retouch", "description": "加急精修", "quantity": 2, "unit_price_cents": 10000},
		},
	}
	var created changeOrder
	if err := c.do(http.MethodPost, "/api/v1/change-orders", c.token, body, &created); err != nil {
		return err
	}
	if created.Status != "draft" || created.AmountCents != 70000 || created.Version != 1 || len(created.Items) != 2 {
		return fmt.Errorf("created change order = %+v", created)
	}
	if created.OrderNo == "" || created.Currency != "CNY" {
		return fmt.Errorf("change order header = %+v", created)
	}
	if err := c.expect(http.MethodPost, "/api/v1/change-orders", c.token, body, 409, "active_change_order"); err != nil {
		return err
	}
	if err := c.expect(http.MethodPost, "/api/v1/payments/orders", c.token,
		map[string]any{"project_id": projectID, "kind": "change"}, 422, "validation_failed"); err != nil {
		return err
	}
	fmt.Println("  draft with 2 items totalling 70000, duplicate -> 409 active_change_order, back-door kind=change -> 422")

	step("M2 change order send")
	sendPath := "/api/v1/change-orders/" + created.ID.String() + "/send"
	if err := c.expect(http.MethodPost, sendPath, c.token,
		map[string]any{"expected_version": created.Version + 1}, 409, "version_conflict"); err != nil {
		return err
	}
	var sent changeOrder
	if err := c.do(http.MethodPost, sendPath, c.token,
		map[string]any{"expected_version": created.Version}, &sent); err != nil {
		return err
	}
	if sent.Status != "awaiting_signature" || sent.SignTaskID == "" || sent.SignURL == "" {
		return fmt.Errorf("sent change order = %+v", sent)
	}
	fmt.Printf("  task=%s status=%s\n", sent.SignTaskID, sent.Status)

	step("M2 change order signing callbacks")
	signingEvent := map[string]any{
		"event_id": "smoke-change-signing-" + tag, "sign_task_id": sent.SignTaskID, "type": "signing",
	}
	if err := c.expectCallback(signingEvent, cfg.ESignSecret, 200, "processed"); err != nil {
		return err
	}
	signedEvent := map[string]any{
		"event_id": "smoke-change-signed-" + tag, "sign_task_id": sent.SignTaskID, "type": "signed",
		"signer_name": "Smoke Client " + tag, "document_sha256": fmt.Sprintf("%064x", time.Now().UnixNano()),
		"signed_at": time.Now().UTC().Format(time.RFC3339),
	}
	if err := c.expectCallback(signedEvent, cfg.ESignSecret, 200, "processed"); err != nil {
		return err
	}
	if err := c.expectCallback(map[string]any{
		"event_id": "smoke-change-forged-" + tag, "sign_task_id": sent.SignTaskID, "type": "signed",
		"signer_name": "Mallory", "document_sha256": fmt.Sprintf("%064x", 1),
		"signed_at": time.Now().UTC().Format(time.RFC3339),
	}, cfg.ESignSecret+"-wrong", 401, "invalid_signature"); err != nil {
		return err
	}
	signed, err := c.getChangeOrder(created.ID)
	if err != nil {
		return err
	}
	if signed.Status == "signed" {
		// The post-commit order creation failed; the retry endpoint must recover.
		if err := c.do(http.MethodPost, "/api/v1/change-orders/"+created.ID.String()+"/payment-order",
			c.token, nil, &signed); err != nil {
			return err
		}
	}
	if signed.Status != "awaiting_payment" || signed.PaymentOrderID == nil || signed.SignedAt == nil {
		return fmt.Errorf("signed change order = %+v", signed)
	}
	if err := c.expectCallback(signedEvent, cfg.ESignSecret, 200, "duplicate"); err != nil {
		return err
	}
	again, err := c.getChangeOrder(created.ID)
	if err != nil {
		return err
	}
	if again.Version != signed.Version {
		return fmt.Errorf("duplicate signed callback changed version: %d -> %d", signed.Version, again.Version)
	}
	fmt.Printf("  signed -> awaiting_payment order=%s v%d, duplicate no-op, forged signature 401\n",
		*signed.PaymentOrderID, signed.Version)

	step("M2 supplement payment")
	var supplement paymentOrder
	if err := c.do(http.MethodGet, "/api/v1/payments/orders/"+signed.PaymentOrderID.String(),
		c.token, nil, &supplement); err != nil {
		return err
	}
	if supplement.Kind != "change" || supplement.Status != "pending" || supplement.AmountCents != 70000 {
		return fmt.Errorf("supplement order = %+v", supplement)
	}
	project, err := c.getProject(projectID)
	if err != nil {
		return err
	}
	if project.ChangeAmountCents != 0 {
		return fmt.Errorf("project change_amount_cents before payment = %d, want 0", project.ChangeAmountCents)
	}
	supplementPaid := map[string]any{
		"event_id": "smoke-change-pay-" + tag, "type": payment.EventPaymentSucceeded,
		"merchant_id": cfg.PaymentMerchant, "out_trade_no": supplement.OutTradeNo,
		"provider_trade_no": "wx-change-" + tag, "amount_cents": 70000, "currency": "CNY",
		"paid_at": time.Now().UTC().Format(time.RFC3339),
	}
	if err := c.expectPaymentCallback(supplementPaid, cfg.PaymentSecret, 200, "processed"); err != nil {
		return err
	}
	if err := c.expectPaymentCallback(supplementPaid, cfg.PaymentSecret, 200, "duplicate"); err != nil {
		return err
	}
	applied, err := c.getChangeOrder(created.ID)
	if err != nil {
		return err
	}
	if applied.Status != "applied" || applied.AppliedAt == nil {
		return fmt.Errorf("applied change order = %+v", applied)
	}
	project, err = c.getProject(projectID)
	if err != nil {
		return err
	}
	if project.ChangeAmountCents != 70000 {
		return fmt.Errorf("project change_amount_cents = %d, want 70000", project.ChangeAmountCents)
	}
	if err := c.expect(http.MethodPost, "/api/v1/change-orders/"+created.ID.String()+"/cancel", c.token,
		map[string]any{"expected_version": applied.Version, "reason": "too late"}, 409, "change_order_paid"); err != nil {
		return err
	}
	fmt.Println("  supplement paid -> applied, project.change_amount_cents=70000, cancelling a paid amendment -> 409 change_order_paid")

	step("M2 change order refunds")
	payments, err := c.listPayments(projectID)
	if err != nil {
		return err
	}
	supplementPayment, ok := paymentByAmount(payments.Items, 70000)
	if !ok {
		return fmt.Errorf("supplement payment for 70000 not found: %+v", payments.Items)
	}
	partial, err := c.createRefund(supplementPayment.ID, 20000, "变更部分退款")
	if err != nil {
		return err
	}
	if err := c.expectPaymentCallback(map[string]any{
		"event_id": "smoke-change-refund-partial-" + tag, "type": payment.EventRefundSucceeded,
		"merchant_id": cfg.PaymentMerchant, "out_refund_no": partial.OutRefundNo,
		"provider_refund_id": partial.ProviderRefundID, "amount_cents": 20000, "currency": "CNY",
	}, cfg.PaymentSecret, 200, "processed"); err != nil {
		return err
	}
	alerts, err := c.listAlerts("/api/v1/payment-alerts?project_id=" + projectID.String() +
		"&code=change_order_partially_refunded&limit=100")
	if err != nil {
		return err
	}
	if alerts.Total != 1 {
		return fmt.Errorf("partial refund alert total = %d, want 1", alerts.Total)
	}
	stillApplied, err := c.getChangeOrder(created.ID)
	if err != nil {
		return err
	}
	if stillApplied.Status != "applied" || stillApplied.Version != applied.Version {
		return fmt.Errorf("partially refunded change order = %+v", stillApplied)
	}
	rest, err := c.createRefund(supplementPayment.ID, 50000, "变更全额退款")
	if err != nil {
		return err
	}
	if err := c.expectPaymentCallback(map[string]any{
		"event_id": "smoke-change-refund-rest-" + tag, "type": payment.EventRefundSucceeded,
		"merchant_id": cfg.PaymentMerchant, "out_refund_no": rest.OutRefundNo,
		"provider_refund_id": rest.ProviderRefundID, "amount_cents": 50000, "currency": "CNY",
	}, cfg.PaymentSecret, 200, "processed"); err != nil {
		return err
	}
	reversed, err := c.getChangeOrder(created.ID)
	if err != nil {
		return err
	}
	if reversed.Status != "reversed" {
		return fmt.Errorf("fully refunded change order = %+v", reversed)
	}
	project, err = c.getProject(projectID)
	if err != nil {
		return err
	}
	if project.ChangeAmountCents != 0 {
		return fmt.Errorf("project change_amount_cents after reversal = %d, want 0", project.ChangeAmountCents)
	}
	alerts, err = c.listAlerts("/api/v1/payment-alerts?project_id=" + projectID.String() +
		"&code=change_order_partially_refunded&limit=100")
	if err != nil {
		return err
	}
	if alerts.Total != 1 {
		return fmt.Errorf("full refund re-raised the partial alert: %d", alerts.Total)
	}
	fmt.Println("  partial refund -> warning alert + stays applied, full refund -> reversed, projection back to 0")

	step("M2 change order cancel closes a live order")
	var second changeOrder
	if err := c.do(http.MethodPost, "/api/v1/change-orders", c.token, map[string]any{
		"project_id": projectID, "reason": "差旅追加",
		"signer_name": "Smoke Client " + tag,
		"items": []map[string]any{
			{"kind": "travel", "description": "外地差旅", "quantity": 1, "unit_price_cents": 30000},
		},
	}, &second); err != nil {
		return err
	}
	var secondSent changeOrder
	if err := c.do(http.MethodPost, "/api/v1/change-orders/"+second.ID.String()+"/send", c.token,
		map[string]any{"expected_version": second.Version}, &secondSent); err != nil {
		return err
	}
	secondTag := tag + "-b"
	if err := c.expectCallback(map[string]any{
		"event_id": "smoke-change-signed-" + secondTag, "sign_task_id": secondSent.SignTaskID, "type": "signed",
		"signer_name": "Smoke Client " + tag, "document_sha256": fmt.Sprintf("%064x", time.Now().UnixNano()+1),
		"signed_at": time.Now().UTC().Format(time.RFC3339),
	}, cfg.ESignSecret, 200, "processed"); err != nil {
		return err
	}
	secondNow, err := c.getChangeOrder(second.ID)
	if err != nil {
		return err
	}
	if secondNow.Status == "signed" {
		if err := c.do(http.MethodPost, "/api/v1/change-orders/"+second.ID.String()+"/payment-order",
			c.token, nil, &secondNow); err != nil {
			return err
		}
	}
	if secondNow.Status != "awaiting_payment" || secondNow.PaymentOrderID == nil {
		return fmt.Errorf("second change order = %+v", secondNow)
	}
	var secondOrder paymentOrder
	if err := c.do(http.MethodGet, "/api/v1/payments/orders/"+secondNow.PaymentOrderID.String(),
		c.token, nil, &secondOrder); err != nil {
		return err
	}
	var cancelled changeOrder
	if err := c.do(http.MethodPost, "/api/v1/change-orders/"+second.ID.String()+"/cancel", c.token,
		map[string]any{"expected_version": secondNow.Version, "reason": "客户改为远程沟通"}, &cancelled); err != nil {
		return err
	}
	if cancelled.Status != "cancelled" || cancelled.VoidReason == "" {
		return fmt.Errorf("cancelled change order = %+v", cancelled)
	}
	var closedOrder paymentOrder
	if err := c.do(http.MethodGet, "/api/v1/payments/orders/"+secondOrder.ID.String(),
		c.token, nil, &closedOrder); err != nil {
		return err
	}
	if closedOrder.Status != "closed" {
		return fmt.Errorf("supplement order after cancel = %s, want closed", closedOrder.Status)
	}
	if err := c.expectPaymentCallback(map[string]any{
		"event_id": "smoke-change-late-" + secondTag, "type": payment.EventPaymentSucceeded,
		"merchant_id": cfg.PaymentMerchant, "out_trade_no": secondOrder.OutTradeNo,
		"provider_trade_no": "wx-change-late-" + secondTag, "amount_cents": 30000, "currency": "CNY",
	}, cfg.PaymentSecret, 200, "ignored"); err != nil {
		return err
	}
	if err := c.expect(http.MethodPost, "/api/v1/change-orders/"+second.ID.String()+"/cancel", c.token,
		map[string]any{"expected_version": cancelled.Version, "reason": "again"}, 409, "illegal_state"); err != nil {
		return err
	}
	var listed changeOrderList
	if err := c.do(http.MethodGet, "/api/v1/change-orders?project_id="+projectID.String()+"&limit=100",
		c.token, nil, &listed); err != nil {
		return err
	}
	if listed.Total != 2 {
		return fmt.Errorf("change order list total = %d, want 2", listed.Total)
	}
	fmt.Println("  cancel closes the live supplement order, late payment -> ignored, repeated cancel -> 409 illegal_state")
	return nil
}

func (c *client) getChangeOrder(id uuid.UUID) (*changeOrder, error) {
	var co changeOrder
	if err := c.do(http.MethodGet, "/api/v1/change-orders/"+id.String(), c.token, nil, &co); err != nil {
		return nil, err
	}
	return &co, nil
}
