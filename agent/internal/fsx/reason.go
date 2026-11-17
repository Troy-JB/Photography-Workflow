package fsx

// Reason names a condition that must pause the whole import instead of failing
// a single file. The values match the agent's persisted pause reasons.
type Reason string

const (
	ReasonCardRemoved Reason = "card_removed"
	ReasonDiskFull    Reason = "disk_full"
	// ReasonDestination covers actionable destination problems: permissions,
	// UNC reachability, write protection and over-long paths.
	ReasonDestination Reason = "destination_error"
	// ReasonAuthRequired means the session expired or was revoked. The import
	// pauses instead of degrading to a local-only copy.
	ReasonAuthRequired Reason = "auth_required"
	// ReasonOffline means the server or storage is unreachable after retries.
	// Local copies stay verified; the run resumes from the uploaded bytes.
	ReasonOffline Reason = "offline"
)

// ClassifySource maps a read-side error to a pause reason, or "" when the file
// alone failed (for example it was deleted while copying).
func ClassifySource(err error) Reason {
	if err == nil {
		return ""
	}
	return sourceReason(err)
}

// ClassifyDestination maps a write/verify-side error to a pause reason.
func ClassifyDestination(err error) Reason {
	if err == nil {
		return ""
	}
	return destinationReason(err)
}
