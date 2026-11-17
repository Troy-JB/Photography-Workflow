package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"photography-workflow/server/internal/esign"
)

type commerceClient struct {
	ID      uuid.UUID `json:"id"`
	Version int       `json:"version"`
}

type commerceLead struct {
	ID        uuid.UUID  `json:"id"`
	Status    string     `json:"status"`
	Version   int        `json:"version"`
	ClientID  *uuid.UUID `json:"client_id"`
	ProjectID *uuid.UUID `json:"project_id"`
}

type commerceProject struct {
	ID                uuid.UUID `json:"id"`
	State             string    `json:"state"`
	Version           int       `json:"version"`
	ContractStatus    string    `json:"contract_status"`
	PaymentStatus     string    `json:"payment_status"`
	TotalCents        int64     `json:"total_amount_cents"`
	DepositCents      int64     `json:"deposit_cents"`
	BalanceCents      int64     `json:"balance_cents"`
	ChangeAmountCents int64     `json:"change_amount_cents"`
}

type commerceQuote struct {
	ID         uuid.UUID       `json:"id"`
	Status     string          `json:"status"`
	Version    int             `json:"version"`
	RowVersion int             `json:"row_version"`
	TotalCents int64           `json:"total_cents"`
	Snapshot   json.RawMessage `json:"snapshot"`
}

type commerceContract struct {
	ID           uuid.UUID `json:"id"`
	Status       string    `json:"status"`
	Version      int       `json:"version"`
	SignTaskID   string    `json:"sign_task_id"`
	SignURL      string    `json:"sign_url"`
	DocumentHash string    `json:"document_hash"`
	SignerName   string    `json:"signer_name"`
}

// runCommerce exercises the M1 closed loop: client, lead follow-up and
// conversion, brief, versioned quote, contract snapshot, provider task and
// signed callbacks that drive the project projection and transition gate.
func runCommerce(c *client, cfg smokeConfig) error {
	tag := time.Now().Format("150405") + "-" + shortRandom()

	step("M1 create client")
	var cl commerceClient
	if err := c.do(http.MethodPost, "/api/v1/clients", c.token, map[string]any{
		"name": "Smoke Client " + tag, "phone": "13800000000", "source": "smoke",
		"tags": []string{"smoke", "commercial"},
	}, &cl); err != nil {
		return err
	}
	fmt.Printf("  client=%s version=%d\n", cl.ID, cl.Version)

	step("M1 create lead and follow up")
	var lead commerceLead
	if err := c.do(http.MethodPost, "/api/v1/leads", c.token, map[string]any{
		"name": "Smoke Lead " + tag, "phone": "13800000000", "source": "smoke",
		"need": "品牌年度拍摄", "budget_min_cents": 100000, "budget_max_cents": 300000,
	}, &lead); err != nil {
		return err
	}
	var followUp struct {
		ID      int64  `json:"id"`
		Summary string `json:"summary"`
	}
	if err := c.do(http.MethodPost, "/api/v1/leads/"+lead.ID.String()+"/follow-ups", c.token, map[string]any{
		"channel": "wechat", "summary": "首轮沟通，确认预算", "next_at": time.Now().Add(24 * time.Hour).Format(time.RFC3339),
	}, &followUp); err != nil {
		return err
	}
	if followUp.ID == 0 {
		return fmt.Errorf("follow-up was not created")
	}
	var leadNow commerceLead
	if err := c.do(http.MethodGet, "/api/v1/leads/"+lead.ID.String(), c.token, nil, &leadNow); err != nil {
		return err
	}
	if leadNow.Version != lead.Version+1 {
		return fmt.Errorf("follow-up did not bump lead version: %d -> %d", lead.Version, leadNow.Version)
	}

	step("M1 convert lead")
	var converted commerceLead
	if err := c.do(http.MethodPost, "/api/v1/leads/"+lead.ID.String()+"/convert", c.token, map[string]any{
		"expected_version": leadNow.Version, "client_id": cl.ID, "create_project": true,
		"project_title":      "Smoke Commercial " + tag,
		"total_amount_cents": 250000, "deposit_cents": 50000,
	}, &converted); err != nil {
		return err
	}
	if converted.Status != "converted" || converted.ClientID == nil || converted.ProjectID == nil {
		return fmt.Errorf("lead conversion = %+v", converted)
	}
	if *converted.ClientID != cl.ID {
		return fmt.Errorf("converted client = %s, want %s", *converted.ClientID, cl.ID)
	}
	projectID := *converted.ProjectID
	fmt.Printf("  lead=%s project=%s client=%s\n", converted.ID, projectID, *converted.ClientID)

	step("M1 project brief")
	var brief struct {
		Version   int    `json:"version"`
		Style     string `json:"style"`
		ShotCount int    `json:"shot_count"`
	}
	if err := c.do(http.MethodPut, "/api/v1/projects/"+projectID.String()+"/brief", c.token, map[string]any{
		"expected_version": 0, "style": "自然光", "shot_count": 120,
		"venue": "棚拍 + 外景", "deliverable_spec": "120 张精修", "usage_scope": "品牌官网/社交媒体",
	}, &brief); err != nil {
		return err
	}
	if brief.Version != 1 || brief.ShotCount != 120 {
		return fmt.Errorf("brief = %+v", brief)
	}
	if err := c.do(http.MethodPut, "/api/v1/projects/"+projectID.String()+"/brief", c.token, map[string]any{
		"expected_version": 1, "style": "自然光 + 低饱和", "shot_count": 130,
	}, &brief); err != nil {
		return err
	}
	if brief.Version != 2 {
		return fmt.Errorf("brief version = %d, want 2", brief.Version)
	}
	if err := c.expect(http.MethodPut, "/api/v1/projects/"+projectID.String()+"/brief", c.token, map[string]any{
		"expected_version": 1, "style": "stale",
	}, 409, "version_conflict"); err != nil {
		return err
	}
	fmt.Println("  brief create -> v1, update -> v2, stale write -> 409 version_conflict")

	step("M1 versioned quote")
	var quote commerceQuote
	if err := c.do(http.MethodPost, "/api/v1/quotes", c.token, map[string]any{
		"project_id": projectID, "deposit_cents": 50000, "terms": "50% 预付款，余款交付前结清",
		"items": []map[string]any{
			{"kind": "service", "description": "品牌主拍", "quantity": 1, "unit_price_cents": 200000},
			{"kind": "retouch", "description": "精修", "quantity": 20, "unit_price_cents": 2500},
		},
	}, &quote); err != nil {
		return err
	}
	if quote.Status != "draft" || quote.TotalCents != 250000 || quote.Version != 1 {
		return fmt.Errorf("quote = %+v", quote)
	}
	var sent commerceQuote
	if err := c.do(http.MethodPost, "/api/v1/quotes/"+quote.ID.String()+"/send", c.token, map[string]any{
		"expected_version": quote.RowVersion,
	}, &sent); err != nil {
		return err
	}
	if sent.Status != "sent" || len(sent.Snapshot) < 10 {
		return fmt.Errorf("sent quote = %+v", sent)
	}
	fmt.Printf("  quote v%d total=%d sent, snapshot frozen\n", sent.Version, sent.TotalCents)

	step("M1 contract from snapshot")
	var contract commerceContract
	if err := c.do(http.MethodPost, "/api/v1/contracts", c.token, map[string]any{
		"project_id": projectID, "quote_id": sent.ID, "signer_name": "Smoke Client " + tag,
	}, &contract); err != nil {
		return err
	}
	if contract.Status != "rendered" || len(contract.DocumentHash) != 64 || contract.Version != 1 {
		return fmt.Errorf("contract = %+v", contract)
	}
	var project commerceProject
	if err := c.do(http.MethodGet, "/api/v1/projects/"+projectID.String(), c.token, nil, &project); err != nil {
		return err
	}
	if project.ContractStatus != "rendered" || project.TotalCents != 250000 || project.BalanceCents != 200000 {
		return fmt.Errorf("project projection after contract = %+v", project)
	}
	if project.Version != 1 {
		return fmt.Errorf("contract creation must not bump project version, got %d", project.Version)
	}
	if err := c.expect(http.MethodPost, "/api/v1/contracts", c.token, map[string]any{
		"project_id": projectID, "quote_id": sent.ID,
	}, 409, "active_contract"); err != nil {
		return err
	}
	fmt.Println("  contract rendered, document hash set, project amounts synced, duplicate -> 409 active_contract")

	step("M1 send contract")
	var signing commerceContract
	if err := c.do(http.MethodPost, "/api/v1/contracts/"+contract.ID.String()+"/send", c.token, map[string]any{
		"expected_version": contract.Version,
	}, &signing); err != nil {
		return err
	}
	if signing.Status != "sent" || signing.SignTaskID == "" || signing.SignURL == "" {
		return fmt.Errorf("sent contract = %+v", signing)
	}
	fmt.Printf("  task=%s status=%s\n", signing.SignTaskID, signing.Status)

	step("M1 esign callbacks")
	eventSigning := map[string]any{
		"event_id": "smoke-signing-" + tag, "sign_task_id": signing.SignTaskID, "type": "signing",
	}
	if err := c.expectCallback(eventSigning, cfg.ESignSecret, 200, "processed"); err != nil {
		return err
	}
	var signingState commerceContract
	if err := c.do(http.MethodGet, "/api/v1/contracts/"+contract.ID.String(), c.token, nil, &signingState); err != nil {
		return err
	}
	if signingState.Status != "signing" || signingState.Version != signing.Version+1 {
		return fmt.Errorf("signing callback state = %+v", signingState)
	}

	eventSigned := map[string]any{
		"event_id": "smoke-signed-" + tag, "sign_task_id": signing.SignTaskID, "type": "signed",
		"signer_name": "Smoke Client " + tag, "document_sha256": strings.Repeat("b", 64),
		"signed_at": time.Now().UTC().Format(time.RFC3339),
	}
	if err := c.expectCallback(eventSigned, cfg.ESignSecret, 200, "processed"); err != nil {
		return err
	}
	var signed commerceContract
	if err := c.do(http.MethodGet, "/api/v1/contracts/"+contract.ID.String(), c.token, nil, &signed); err != nil {
		return err
	}
	if signed.Status != "signed" || signed.Version != signingState.Version+1 {
		return fmt.Errorf("signed callback state = %+v", signed)
	}
	if err := c.expectCallback(eventSigned, cfg.ESignSecret, 200, "duplicate"); err != nil {
		return err
	}
	var afterDuplicate commerceContract
	if err := c.do(http.MethodGet, "/api/v1/contracts/"+contract.ID.String(), c.token, nil, &afterDuplicate); err != nil {
		return err
	}
	if afterDuplicate.Version != signed.Version {
		return fmt.Errorf("duplicate callback changed contract version: %d -> %d", signed.Version, afterDuplicate.Version)
	}
	tampered := map[string]any{
		"event_id": "smoke-tampered-" + tag, "sign_task_id": signing.SignTaskID, "type": "signed", "signer_name": "Mallory",
	}
	if err := c.expectCallback(tampered, cfg.ESignSecret+"-wrong", 401, "invalid_signature"); err != nil {
		return err
	}
	unknown := map[string]any{
		"event_id": "smoke-unknown-" + tag, "sign_task_id": "sbx_unknown_task", "type": "signed",
		"signer_name": "Nobody", "document_sha256": strings.Repeat("c", 64),
		"signed_at": time.Now().UTC().Format(time.RFC3339),
	}
	if err := c.expectCallback(unknown, cfg.ESignSecret, 200, "ignored"); err != nil {
		return err
	}
	noHash := map[string]any{
		"event_id": "smoke-nohash-" + tag, "sign_task_id": signing.SignTaskID, "type": "signed",
		"signer_name": "Smoke Client " + tag,
	}
	if err := c.expectCallback(noHash, cfg.ESignSecret, 400, "malformed_callback"); err != nil {
		return err
	}
	fmt.Println("  signing -> signed, duplicate -> version unchanged, bad signature -> 401, no document hash -> 400, unknown task -> ignored")

	step("M1 contracted gate")
	var projectAfter commerceProject
	if err := c.do(http.MethodGet, "/api/v1/projects/"+projectID.String(), c.token, nil, &projectAfter); err != nil {
		return err
	}
	if projectAfter.ContractStatus != "signed" {
		return fmt.Errorf("project contract_status = %s, want signed", projectAfter.ContractStatus)
	}
	transitions := "/api/v1/projects/" + projectID.String() + "/transitions"
	version := projectAfter.Version
	for _, to := range []string{"quoted", "contract_pending", "contracted"} {
		var p commerceProject
		if err := c.do(http.MethodPost, transitions, c.token,
			map[string]any{"to": to, "expected_version": version}, &p); err != nil {
			return err
		}
		version = p.Version
		if to == "contracted" && (p.State != "contracted" || p.ContractStatus != "signed") {
			return fmt.Errorf("project after contracted = %+v", p)
		}
	}

	step("M1 active contract blocks new quote sends")
	var draft commerceQuote
	if err := c.do(http.MethodPost, "/api/v1/quotes", c.token, map[string]any{
		"project_id": projectID, "items": []map[string]any{
			{"description": "追加拍摄", "quantity": 1, "unit_price_cents": 30000},
		},
	}, &draft); err != nil {
		return err
	}
	if err := c.expect(http.MethodPost, "/api/v1/quotes/"+draft.ID.String()+"/send", c.token,
		map[string]any{"expected_version": draft.RowVersion}, 409, "active_contract"); err != nil {
		return err
	}
	fmt.Println("  project contracted after signed callback; sending a new quote -> 409 active_contract")
	return runPayments(c, cfg, projectID, tag)
}

// expectCallback signs the payload with the sandbox scheme and asserts the
// returned status value.
func (c *client) expectCallback(body map[string]any, secret string, wantStatus int, wantResult string) error {
	return c.expectWebhook("/webhooks/esign/sandbox", esign.SignRequestBody, body, secret, wantStatus, wantResult)
}

func (c *client) expectWebhook(path string, sign func(string, time.Time, []byte) string, body map[string]any, secret string, wantStatus int, wantResult string) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.base+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-PWF-Signature", sign(secret, time.Now(), raw))
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respRaw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != wantStatus {
		return fmt.Errorf("callback %s: status %d, want %d: %s", body["event_id"], resp.StatusCode, wantStatus, string(respRaw))
	}
	if wantResult != "" {
		if wantStatus >= 400 {
			var p struct {
				Code string `json:"code"`
			}
			_ = json.Unmarshal(respRaw, &p)
			if p.Code != wantResult {
				return fmt.Errorf("callback %s: code %q, want %q: %s", body["event_id"], p.Code, wantResult, string(respRaw))
			}
			return nil
		}
		var out struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(respRaw, &out)
		if out.Status != wantResult {
			return fmt.Errorf("callback %s: status %q, want %q: %s", body["event_id"], out.Status, wantResult, string(respRaw))
		}
	}
	return nil
}
