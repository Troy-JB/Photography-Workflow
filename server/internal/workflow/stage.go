package workflow

type Stage string

const (
	StageNotShot         Stage = "not_shot"
	StageShooting        Stage = "shooting"
	StageOriginals       Stage = "originals_delivery"
	StageSelecting       Stage = "selecting"
	StageRetouchDelivery Stage = "retouch_delivery"
	StageCompleted       Stage = "completed"
	StageCancelled       Stage = "cancelled"
)

var stages = []Stage{
	StageNotShot, StageShooting, StageOriginals, StageSelecting, StageRetouchDelivery, StageCompleted,
}

func Stages() []Stage { return append([]Stage(nil), stages...) }

type Milestones struct {
	ProjectState    string
	PaymentStatus   string
	ShootDatePassed bool
	GalleryStatus   string
	SelectedCount   int
	Submitted       bool
	ProofStatus     string
	DeliveryStatus  string
}

// DeriveStage computes the customer-facing stage from existing module data.
// Cancellation wins over every milestone; no independent workflow status is stored.
func DeriveStage(m Milestones) Stage {
	if m.ProjectState == "cancelled" {
		return StageCancelled
	}
	if m.PaymentStatus == "paid" && m.DeliveryStatus == "ready" {
		return StageCompleted
	}
	if m.PaymentStatus != "deposit_paid" && m.PaymentStatus != "balance_pending" &&
		m.PaymentStatus != "change_pending" && m.PaymentStatus != "paid" {
		return StageNotShot
	}
	if m.Submitted || m.ProofStatus != "" || m.DeliveryStatus != "" {
		return StageRetouchDelivery
	}
	if m.SelectedCount > 0 {
		return StageSelecting
	}
	if m.GalleryStatus == "published" || m.ShootDatePassed {
		return StageOriginals
	}
	return StageShooting
}

func StageLabel(stage Stage) string {
	switch stage {
	case StageNotShot:
		return "未拍摄"
	case StageShooting:
		return "拍摄中"
	case StageOriginals:
		return "原片交付中"
	case StageSelecting:
		return "选片中"
	case StageRetouchDelivery:
		return "精修交付中"
	case StageCompleted:
		return "已完成"
	case StageCancelled:
		return "已取消"
	default:
		return "未拍摄"
	}
}
