package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
)

type agentDevice struct {
	ID           uuid.UUID  `json:"id"`
	Name         string     `json:"name"`
	Fingerprint  string     `json:"fingerprint"`
	Platform     string     `json:"platform"`
	AgentVersion string     `json:"agent_version"`
	Status       string     `json:"status"`
	LastSeenAt   *time.Time `json:"last_seen_at"`
}

type agentHeartbeat struct {
	DeviceID        uuid.UUID       `json:"device_id"`
	ServerTime      time.Time       `json:"server_time"`
	MinAgentVersion string          `json:"min_agent_version"`
	Features        map[string]bool `json:"features"`
}

type batchStatus struct {
	Batch struct {
		ID        uuid.UUID `json:"id"`
		Status    string    `json:"status"`
		FileCount int       `json:"file_count"`
		Bytes     int64     `json:"bytes"`
	} `json:"batch"`
	Assets struct {
		Total      int `json:"total"`
		Registered int `json:"registered"`
		Uploaded   int `json:"uploaded"`
		Verified   int `json:"verified"`
		Missing    int `json:"missing"`
		Corrupt    int `json:"corrupt"`
	} `json:"assets"`
	VerifyJob *struct {
		ID     uuid.UUID `json:"id"`
		Status string    `json:"status"`
	} `json:"verify_job"`
}

// runAgent exercises the device ledger: idempotent registration, heartbeat
// feature advertisement and the unknown-device error path.
func runAgent(c *client) (uuid.UUID, error) {
	step("agent device registration")
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return uuid.Nil, fmt.Errorf("generate agent key: %w", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return uuid.Nil, fmt.Errorf("marshal agent key: %w", err)
	}
	body := map[string]any{
		"name":          "smoke workstation",
		"fingerprint":   "smoke-device-" + shortRandom(),
		"platform":      "windows",
		"agent_version": "0.4.0-smoke",
		"public_key":    base64.StdEncoding.EncodeToString(der),
	}
	if err := c.expect(http.MethodPost, "/agent/v1/devices", "", body, 401, "unauthorized"); err != nil {
		return uuid.Nil, err
	}

	status, raw, err := c.doStatusHeaders(http.MethodPost, "/agent/v1/devices", c.token, nil, body)
	if err != nil {
		return uuid.Nil, err
	}
	if status != http.StatusCreated {
		return uuid.Nil, fmt.Errorf("register device: status %d, want 201: %s", status, string(raw))
	}
	var dev agentDevice
	if err := json.Unmarshal(raw, &dev); err != nil {
		return uuid.Nil, fmt.Errorf("decode device: %w", err)
	}
	if dev.ID == uuid.Nil || dev.Status != "active" || dev.Fingerprint == "" {
		return uuid.Nil, fmt.Errorf("unexpected device = %+v", dev)
	}
	fmt.Printf("  %s fingerprint=%s status=%s\n", dev.ID, dev.Fingerprint, dev.Status)

	step("agent device re-registration")
	var again agentDevice
	if err := c.do(http.MethodPost, "/agent/v1/devices", c.token, body, &again); err != nil {
		return uuid.Nil, err
	}
	if again.ID != dev.ID {
		return uuid.Nil, fmt.Errorf("re-registration created a new device: %s -> %s", dev.ID, again.ID)
	}
	fmt.Println("  same fingerprint returns the same device")

	step("agent heartbeat")
	var hb agentHeartbeat
	if err := c.do(http.MethodPost, "/agent/v1/heartbeats", c.token,
		map[string]any{"device_id": dev.ID, "agent_version": "0.4.0-smoke"}, &hb); err != nil {
		return uuid.Nil, err
	}
	if hb.DeviceID != dev.ID || hb.ServerTime.IsZero() {
		return uuid.Nil, fmt.Errorf("unexpected heartbeat = %+v", hb)
	}
	if !hb.Features["ingest_v1"] {
		return uuid.Nil, fmt.Errorf("heartbeat features = %+v, want ingest_v1", hb.Features)
	}
	fmt.Printf("  server_time=%s min_version=%q features=%v\n",
		hb.ServerTime.Format(time.RFC3339), hb.MinAgentVersion, hb.Features)

	step("agent heartbeat unknown device")
	if err := c.expect(http.MethodPost, "/agent/v1/heartbeats", c.token,
		map[string]any{"device_id": uuid.New()}, 404, "device_not_found"); err != nil {
		return uuid.Nil, err
	}
	fmt.Println("  unknown device -> 404 device_not_found")
	return dev.ID, nil
}

// checkBatchStatus polls the agent reconciliation view after verification. The
// counts must match the single-file fixture and the job must be the one the
// finalize call returned.
func checkBatchStatus(c *client, batchID, jobID uuid.UUID) error {
	step("agent batch status")
	var st batchStatus
	if err := c.do(http.MethodGet, "/agent/v1/ingest-batches/"+batchID.String(), c.token, nil, &st); err != nil {
		return err
	}
	if st.Batch.ID != batchID || st.Batch.Status != "completed" {
		return fmt.Errorf("batch status = %s id=%s, want completed", st.Batch.Status, st.Batch.ID)
	}
	if st.Assets.Total != 1 || st.Assets.Verified != 1 || st.Assets.Missing != 0 || st.Assets.Corrupt != 0 {
		return fmt.Errorf("asset counts = %+v, want 1 verified", st.Assets)
	}
	if st.VerifyJob == nil || st.VerifyJob.ID != jobID || st.VerifyJob.Status != "succeeded" {
		return fmt.Errorf("verify job = %+v, want %s succeeded", st.VerifyJob, jobID)
	}
	fmt.Printf("  status=%s total=%d verified=%d job=%s\n",
		st.Batch.Status, st.Assets.Total, st.Assets.Verified, st.VerifyJob.ID)

	if err := c.expect(http.MethodGet, "/agent/v1/ingest-batches/"+uuid.New().String(),
		c.token, nil, 404, "not_found"); err != nil {
		return err
	}
	fmt.Println("  unknown batch -> 404 not_found")
	return nil
}
