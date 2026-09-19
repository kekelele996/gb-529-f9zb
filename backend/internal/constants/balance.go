package constants

type BalanceStatus string

const (
	BalanceQueued                BalanceStatus = "queued"
	BalanceCalculating           BalanceStatus = "calculating"
	BalancePendingReview         BalanceStatus = "pending_review"
	BalanceAccepted              BalanceStatus = "accepted"
	BalanceRejected              BalanceStatus = "rejected"
	BalanceInvalidated           BalanceStatus = "invalidated"
	BalanceRecalculationRequired BalanceStatus = "recalculation_required"
	BalanceReplaced              BalanceStatus = "replaced"
)

var BalanceStatuses = []BalanceStatus{
	BalanceQueued, BalanceCalculating, BalancePendingReview,
	BalanceAccepted, BalanceRejected, BalanceInvalidated,
	BalanceRecalculationRequired, BalanceReplaced,
}

func ValidBalanceStatus(value BalanceStatus) bool {
	for _, status := range BalanceStatuses {
		if status == value {
			return true
		}
	}
	return false
}

// EvidenceGateActiveStatuses 列出运行存证后仍对边界证据完整性闸门开放的状态：
// 已计算、待复核以及已经挂起重算但又出现新证据的运行。终态运行不再被闸门改写。
var EvidenceGateActiveStatuses = []BalanceStatus{
	BalanceCalculating, BalancePendingReview, BalanceRecalculationRequired,
}

func CanTransitionBalance(from, to BalanceStatus) bool {
	switch from {
	case BalanceQueued:
		return to == BalanceCalculating || to == BalanceInvalidated
	case BalanceCalculating:
		return to == BalancePendingReview || to == BalanceInvalidated || to == BalanceRecalculationRequired
	case BalancePendingReview:
		return to == BalanceAccepted || to == BalanceRejected || to == BalanceInvalidated || to == BalanceRecalculationRequired
	case BalanceRejected:
		return to == BalanceInvalidated
	case BalanceRecalculationRequired:
		return to == BalanceInvalidated || to == BalanceReplaced
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
