package api

import (
	"encoding/json"
	"time"
)

// ---------- auth ----------

type LoginRequest struct {
	Email      string `json:"email"`
	Password   string `json:"password"`
	TenantSlug string `json:"tenant_slug,omitempty"`
}

type RefreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

type User struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

type Tenant struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// Session is issued by login and rotated by refresh.
type Session struct {
	AccessToken      string    `json:"access_token"`
	AccessExpiresAt  time.Time `json:"access_expires_at"`
	RefreshToken     string    `json:"refresh_token"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at"`
	User             User      `json:"user"`
	Tenant           Tenant    `json:"tenant"`
	Role             string    `json:"role"`
}

type Profile struct {
	User   User   `json:"user"`
	Tenant Tenant `json:"tenant"`
	Role   string `json:"role"`
}

// ---------- projects ----------

type Project struct {
	ID        string    `json:"id"`
	Code      string    `json:"code"`
	Title     string    `json:"title"`
	State     string    `json:"state"`
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type ProjectList struct {
	Items  []Project `json:"items"`
	Total  int       `json:"total"`
	Limit  int       `json:"limit"`
	Offset int       `json:"offset"`
}

// ---------- devices ----------

type DeviceRegistrationRequest struct {
	Name         string `json:"name"`
	Fingerprint  string `json:"fingerprint"`
	Platform     string `json:"platform,omitempty"`
	AgentVersion string `json:"agent_version"`
	PublicKey    string `json:"public_key"`
}

type Device struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Fingerprint  string     `json:"fingerprint"`
	Platform     string     `json:"platform"`
	AgentVersion string     `json:"agent_version"`
	Status       string     `json:"status"`
	LastSeenAt   *time.Time `json:"last_seen_at"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

type HeartbeatRequest struct {
	DeviceID     string `json:"device_id"`
	AgentVersion string `json:"agent_version,omitempty"`
}

type HeartbeatResponse struct {
	DeviceID        string          `json:"device_id"`
	ServerTime      time.Time       `json:"server_time"`
	MinAgentVersion string          `json:"min_agent_version"`
	Features        map[string]bool `json:"features"`
}

// ---------- ingest ----------

type Batch struct {
	ID              string     `json:"id"`
	ProjectID       string     `json:"project_id"`
	DeviceID        string     `json:"device_id"`
	CardFingerprint string     `json:"card_fingerprint"`
	FileCount       int        `json:"file_count"`
	Bytes           int64      `json:"bytes"`
	Status          string     `json:"status"`
	CreatedAt       time.Time  `json:"created_at"`
	FinishedAt      *time.Time `json:"finished_at"`
}

type CreateBatchRequest struct {
	ProjectID       string `json:"project_id"`
	DeviceID        string `json:"device_id,omitempty"`
	CardFingerprint string `json:"card_fingerprint,omitempty"`
}

// FileInput mirrors the server contract. Exif is the compact digest, never the
// full ExifTool dump.
type FileInput struct {
	RelPath    string          `json:"rel_path"`
	Size       int64           `json:"size"`
	XXH64      string          `json:"xxh64,omitempty"`
	SHA256     string          `json:"sha256"`
	Mime       string          `json:"mime,omitempty"`
	Width      int             `json:"width,omitempty"`
	Height     int             `json:"height,omitempty"`
	CapturedAt *time.Time      `json:"captured_at,omitempty"`
	Camera     string          `json:"camera,omitempty"`
	Lens       string          `json:"lens,omitempty"`
	Exif       json.RawMessage `json:"exif,omitempty"`
	IsVideo    bool            `json:"is_video,omitempty"`
}

type AssetResult struct {
	RelPath    string `json:"rel_path"`
	AssetID    string `json:"asset_id"`
	Duplicated bool   `json:"duplicated"`
	Status     string `json:"status"`
}

type RegisterResult struct {
	Accepted   int           `json:"accepted"`
	Inserted   int           `json:"inserted"`
	Duplicated int           `json:"duplicated"`
	Assets     []AssetResult `json:"assets"`
}

type PresignRequest struct {
	AssetID  string `json:"asset_id"`
	Size     int64  `json:"size"`
	PartSize int64  `json:"part_size,omitempty"`
}

type PresignedPart struct {
	PartNumber int32  `json:"part_number"`
	URL        string `json:"url"`
}

type UploadPlan struct {
	Mode      string          `json:"mode"`
	AssetID   string          `json:"asset_id"`
	ObjectKey string          `json:"object_key"`
	URL       string          `json:"url,omitempty"`
	UploadID  string          `json:"upload_id,omitempty"`
	PartSize  int64           `json:"part_size,omitempty"`
	Parts     []PresignedPart `json:"parts,omitempty"`
	ExpiresAt time.Time       `json:"expires_at"`
}

type CompletePart struct {
	PartNumber int32  `json:"part_number"`
	ETag       string `json:"etag"`
}

type CompleteUploadRequest struct {
	UploadID string         `json:"upload_id,omitempty"`
	Parts    []CompletePart `json:"parts,omitempty"`
}

type Asset struct {
	ID          string    `json:"id"`
	ProjectID   string    `json:"project_id"`
	Filename    string    `json:"filename"`
	ContentHash string    `json:"content_hash"`
	OriginalKey string    `json:"original_key"`
	SizeBytes   int64     `json:"size_bytes"`
	Mime        string    `json:"mime"`
	Width       int       `json:"width"`
	Height      int       `json:"height"`
	Status      string    `json:"status"`
	IsVideo     bool      `json:"is_video"`
	CreatedAt   time.Time `json:"created_at"`
}

type AssetList struct {
	Items  []Asset `json:"items"`
	Total  int     `json:"total"`
	Limit  int     `json:"limit"`
	Offset int     `json:"offset"`
}

type Job struct {
	ID          string         `json:"id"`
	Kind        string         `json:"kind"`
	Status      string         `json:"status"`
	SubjectType string         `json:"subject_type"`
	SubjectID   *string        `json:"subject_id"`
	Attempts    int            `json:"attempts"`
	MaxAttempts int            `json:"max_attempts"`
	Payload     map[string]any `json:"payload"`
	Result      map[string]any `json:"result"`
	LastError   string         `json:"last_error"`
	CreatedAt   time.Time      `json:"created_at"`
	FinishedAt  *time.Time     `json:"finished_at"`
}

// VerifyResult is the payload of a finished ingest.verify job.
type VerifyResult struct {
	FileCount int   `json:"file_count"`
	Bytes     int64 `json:"bytes"`
	Verified  int   `json:"verified"`
	Missing   int   `json:"missing"`
	Corrupt   int   `json:"corrupt"`
}

// ParseVerifyResult decodes the job result. A missing result means the worker
// did not finish; callers treat the zero value as "nothing verified".
func ParseVerifyResult(job *Job) (VerifyResult, bool) {
	var out VerifyResult
	if job == nil || job.Result == nil {
		return out, false
	}
	data, err := json.Marshal(job.Result)
	if err != nil {
		return out, false
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, false
	}
	return out, true
}

// ---------- derivatives ----------

// DerivativePresignRequest asks for a PUT slot for an agent-rendered gallery
// preview. The server addresses the object by the preview's own SHA-256, so the
// same JPEG always maps to one key and a retry overwrites nothing.
type DerivativePresignRequest struct {
	AssetID string `json:"asset_id"`
	Kind    string `json:"kind"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
}

type DerivativePresign struct {
	Key       string    `json:"key"`
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

type DerivativeCompleteRequest struct {
	Kind   string `json:"kind"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

type Derivative struct {
	AssetID string `json:"asset_id"`
	Kind    string `json:"kind"`
	Key     string `json:"key"`
	Width   int    `json:"width"`
	Height  int    `json:"height"`
	Bytes   int64  `json:"bytes"`
	Status  string `json:"status"`
}

type AssetCounts struct {
	Total      int `json:"total"`
	Registered int `json:"registered"`
	Uploaded   int `json:"uploaded"`
	Verified   int `json:"verified"`
	Missing    int `json:"missing"`
	Corrupt    int `json:"corrupt"`
}

type BatchStatus struct {
	Batch     Batch       `json:"batch"`
	Assets    AssetCounts `json:"assets"`
	VerifyJob *Job        `json:"verify_job"`
}
