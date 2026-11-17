// Package model holds the agent's persisted domain types. It is deliberately
// free of I/O so the scanner, copy engine, store and CLI can share it.
package model

import "time"

type FileState string

const (
	StateDiscovered     FileState = "discovered"
	StateCopying        FileState = "copying"
	StateCopied         FileState = "copied"
	StateLocalVerified  FileState = "local_verified"
	StateRegistered     FileState = "registered"
	StateUploading      FileState = "uploading"
	StateUploaded       FileState = "uploaded"
	StateRemoteVerified FileState = "remote_verified"
	StateComplete       FileState = "complete"

	StateCopyFailed    FileState = "copy_failed"
	StateHashMismatch  FileState = "hash_mismatch"
	StateUploadFailed  FileState = "upload_failed"
	StateRemoteCorrupt FileState = "remote_corrupt"
)

// LocallyDone reports whether both (or the single configured) local copies
// have been verified against the source hash.
func (s FileState) LocallyDone() bool {
	switch s {
	case StateLocalVerified, StateRegistered, StateUploading, StateUploaded,
		StateRemoteVerified, StateComplete:
		return true
	}
	return false
}

func (s FileState) RemotelyDone() bool {
	return s == StateRemoteVerified || s == StateComplete
}

type ImportStatus string

const (
	// Open means the copy phase is still running or has files that need
	// attention. PauseReason is orthogonal and non-empty while paused.
	ImportOpen ImportStatus = "open"
	// ImportLocalComplete means every enabled local destination verified.
	ImportLocalComplete ImportStatus = "local_complete"
	ImportUploading     ImportStatus = "uploading"
	ImportComplete      ImportStatus = "complete"
	ImportFailed        ImportStatus = "failed"
)

type FileKind string

const (
	KindPhoto   FileKind = "photo"
	KindVideo   FileKind = "video"
	KindSidecar FileKind = "sidecar"
	KindOther   FileKind = "other"
)

type FileRole string

const (
	RoleRAW     FileRole = "raw"
	RoleJPEG    FileRole = "jpeg"
	RoleImage   FileRole = "image"
	RoleVideo   FileRole = "video"
	RoleSidecar FileRole = "sidecar"
	RoleOther   FileRole = "other"
)

const (
	DestA = "A"
	DestB = "B"
)

type Card struct {
	ID          string    `json:"id"`
	Fingerprint string    `json:"fingerprint"`
	VolumeGUID  string    `json:"volume_guid"`
	Label       string    `json:"label"`
	Capacity    int64     `json:"capacity"`
	FirstSeen   time.Time `json:"first_seen"`
	LastSeen    time.Time `json:"last_seen"`
}

type Import struct {
	ID            string       `json:"id"`
	ProjectID     string       `json:"project_id"`
	ProjectCode   string       `json:"project_code"`
	ServerBatchID string       `json:"server_batch_id"`
	CardID        string       `json:"card_id"`
	SourceRoot    string       `json:"source_root"`
	ShootDate     string       `json:"shoot_date"`
	DestA         string       `json:"dest_a"`
	DestB         string       `json:"dest_b"`
	RequireBackup bool         `json:"require_backup"`
	Status        ImportStatus `json:"status"`
	PauseReason   string       `json:"pause_reason"`
	CreatedAt     time.Time    `json:"created_at"`
	CompletedAt   *time.Time   `json:"completed_at,omitempty"`
}

type File struct {
	ID            string     `json:"id"`
	ImportID      string     `json:"import_id"`
	RelPath       string     `json:"rel_path"`
	Size          int64      `json:"size"`
	MtimeNS       int64      `json:"mtime_ns"`
	Kind          FileKind   `json:"kind"`
	Role          FileRole   `json:"role"`
	PairKey       string     `json:"pair_key,omitempty"`
	SHA256        string     `json:"sha256,omitempty"`
	Mime          string     `json:"mime,omitempty"`
	Width         int        `json:"width,omitempty"`
	Height        int        `json:"height,omitempty"`
	CapturedAt    *time.Time `json:"captured_at,omitempty"`
	Camera        string     `json:"camera,omitempty"`
	Lens          string     `json:"lens,omitempty"`
	ExifJSON      string     `json:"exif_json,omitempty"`
	State         FileState  `json:"state"`
	ServerAssetID string     `json:"server_asset_id,omitempty"`
	RemoteBatchID string     `json:"remote_batch_id,omitempty"`
	Error         string     `json:"error,omitempty"`
}

// FileMetadata is the derived information stored once a copy is verified.
// ExifJSON is a compact digest of the camera tags (never the raw ExifTool
// dump) capped at 4 KiB.
type FileMetadata struct {
	MIME       string     `json:"mime,omitempty"`
	Width      int        `json:"width,omitempty"`
	Height     int        `json:"height,omitempty"`
	CapturedAt *time.Time `json:"captured_at,omitempty"`
	Camera     string     `json:"camera,omitempty"`
	Lens       string     `json:"lens,omitempty"`
	ExifJSON   string     `json:"exif_json,omitempty"`
}

type Copy struct {
	FileID    string `json:"file_id"`
	Dest      string `json:"dest"`
	FinalPath string `json:"final_path,omitempty"`
	TempPath  string `json:"temp_path,omitempty"`
	State     string `json:"state"`
	Bytes     int64  `json:"bytes"`
	SHA256    string `json:"sha256,omitempty"`
	Error     string `json:"error,omitempty"`
}

const (
	CopyPending  = "pending"
	CopyCopying  = "copying"
	CopyCopied   = "copied"
	CopyVerified = "verified"
	CopyFailed   = "failed"
)

// UploadState tracks the remote upload of one file. A pending upload has no
// usable session; active means a presigned plan (single or multipart) is in
// flight; completed means the complete call succeeded and the server holds the
// object until verification runs.
const (
	UploadPending   = "pending"
	UploadActive    = "active"
	UploadCompleted = "completed"
)

type Upload struct {
	FileID     string     `json:"file_id"`
	AssetID    string     `json:"asset_id"`
	Mode       string     `json:"mode"`
	ObjectKey  string     `json:"object_key"`
	UploadID   string     `json:"upload_id,omitempty"`
	PartSize   int64      `json:"part_size,omitempty"`
	TotalParts int32      `json:"total_parts,omitempty"`
	Epoch      int        `json:"epoch"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	State      string     `json:"state"`
}

const (
	PartPending  = "pending"
	PartUploaded = "uploaded"
)

// DerivativeState tracks the agent-side rendering and upload of one preview
// kind. uploaded means the server re-hashed the object and accepted it.
const (
	DerivativePending   = "pending"
	DerivativeGenerated = "generating"
	DerivativeUploading = "uploading"
	DerivativeUploaded  = "uploaded"
	DerivativeFailed    = "failed"
)

const DerivativeKindGallery = "gallery"

type Derivative struct {
	FileID    string `json:"file_id"`
	Kind      string `json:"kind"`
	Path      string `json:"path,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	Size      int64  `json:"size,omitempty"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
	State     string `json:"state"`
	Error     string `json:"error,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// UploadPart is the durable resume record for one multipart chunk. The ETag is
// written only after the storage service confirmed the bytes, so a restart can
// skip every part that already landed.
type UploadPart struct {
	FileID string `json:"file_id"`
	Number int32  `json:"part_number"`
	Offset int64  `json:"offset"`
	Size   int64  `json:"size"`
	ETag   string `json:"etag,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	State  string `json:"state"`
}

type Event struct {
	ID           int64     `json:"id"`
	Level        string    `json:"level"`
	Code         string    `json:"code"`
	ImportID     string    `json:"import_id,omitempty"`
	FileID       string    `json:"file_id,omitempty"`
	Message      string    `json:"message"`
	Acknowledged bool      `json:"acknowledged"`
	CreatedAt    time.Time `json:"created_at"`
}
