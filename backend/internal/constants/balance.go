package constants

type BalanceStatus string

const (
	BalanceQueued              BalanceStatus = "queued"
	BalanceCalculating         BalanceStatus = "calculating"
	BalancePendingReview       BalanceStatus = "pending_review"
	BalanceAccepted            BalanceStatus = "accepted"
	BalanceRejected            BalanceStatus = "rejected"
	BalanceInvalidated         BalanceStatus = "invalidated"
	BalanceRecalculateRequired BalanceStatus = "recalculate_required"
	BalanceSuperseded          BalanceStatus = "superseded"
)

var BalanceStatuses = []BalanceStatus{
	BalanceQueued, BalanceCalculating, BalancePendingReview,
	BalanceAccepted, BalanceRejected, BalanceInvalidated,
	BalanceRecalculateRequired, BalanceSuperseded,
}

// BoundaryGateOpenStatuses 是边界证据完整性闸门允许置为待重算的非终态状态。
var BoundaryGateOpenStatuses = []BalanceStatus{
	BalanceCalculating, BalancePendingReview, BalanceRecalculateRequired,
}

func ValidBalanceStatus(value BalanceStatus) bool {
	for _, status := range BalanceStatuses {
		if status == value {
			return true
		}
	}
	return false
}

func CanTransitionBalance(from, to BalanceStatus) bool {
	switch from {
	case BalanceQueued:
		return to == BalanceCalculating || to == BalanceInvalidated
	case BalanceCalculating:
		return to == BalancePendingReview || to == BalanceInvalidated || to == BalanceRecalculateRequired
	case BalancePendingReview:
		return to == BalanceAccepted || to == BalanceRejected || to == BalanceInvalidated || to == BalanceRecalculateRequired
	case BalanceRecalculateRequired:
		return to == BalanceSuperseded || to == BalanceRejected || to == BalanceInvalidated
	case BalanceRejected:
		return to == BalanceInvalidated
	default:
		return false
	}
}

func BalanceStatusValues() []string {
	values := make([]string, 0, len(BalanceStatuses))
	for _, status := range BalanceStatuses {
		values = append(values, string(status))
	}
	return values
}

// 边界证据完整性闸门的待重算原因代码。
const (
	RecalcReasonBoundarySnapshotCloser = "BOUNDARY_SNAPSHOT_CLOSER"
	RecalcReasonLateTransferConfirmed  = "LATE_TRANSFER_CONFIRMED"
	RecalcReasonCoefficientVersion     = "COEFFICIENT_VERSION_CHANGED"
)
