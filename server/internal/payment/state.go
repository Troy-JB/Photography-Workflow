package payment

const (
	KindDeposit        = "deposit"
	KindBalance        = "balance"
	KindFull           = "full"
	KindChange         = "change"
	KindSelectionExtra = "selection_extra"
)

const (
	OrderCreating = "creating"
	OrderPending  = "pending"
	OrderPaid     = "paid"
	OrderClosed   = "closed"
)

const (
	RefundPending   = "pending"
	RefundSucceeded = "succeeded"
	RefundFailed    = "failed"
)

// Project payment projections (projects.payment_status).
const (
	StatusUnpaid         = "unpaid"
	StatusDepositPending = "deposit_pending"
	StatusDepositPaid    = "deposit_paid"
	StatusBalancePending = "balance_pending"
	StatusChangePending  = "change_pending"
	StatusPaid           = "paid"
)

func ValidKind(kind string) bool {
	switch kind {
	case KindDeposit, KindBalance, KindFull, KindChange, KindSelectionExtra:
		return true
	}
	return false
}

// DeriveProjectStatus recomputes the project projection from authoritative
// rows. The contract amount and the applied change-order sum are separate
// receivables:
//
//	net     = all payments - succeeded refunds
//	coreNet = payments for deposit/balance/full orders - their refunds
//
// A change order is only applied once its own payment settled, so net can only
// exceed coreNet by applied change money.
func DeriveProjectStatus(total, effective, deposit, net, coreNet int64, pendingDeposit, pendingFull, pendingBalance bool) string {
	if total <= 0 {
		return StatusUnpaid
	}
	if net < 0 {
		net = 0
	}
	if coreNet < 0 {
		coreNet = 0
	}
	if effective < total {
		effective = total
	}
	if net >= effective {
		return StatusPaid
	}
	if deposit > 0 {
		if coreNet < deposit {
			if pendingDeposit {
				return StatusDepositPending
			}
			return StatusUnpaid
		}
		if coreNet < total {
			if pendingBalance {
				return StatusBalancePending
			}
			return StatusDepositPaid
		}
	} else {
		// No deposit configured: the core is settled by a single full payment.
		if coreNet < total {
			if pendingFull {
				return StatusDepositPending
			}
			return StatusUnpaid
		}
	}
	return StatusChangePending
}
