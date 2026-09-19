package dto

import (
	"strconv"
	"time"

	"lng-boiloff-gas-balance/backend/internal/constants"
)

type RunBalanceRequest struct {
	TankID      uint       `json:"tank_id" binding:"required"`
	PeriodStart *time.Time `json:"period_start" binding:"required"`
	PeriodEnd   *time.Time `json:"period_end" binding:"required"`
}

type SubmitBalanceRequest struct {
	Version uint `json:"version" binding:"required"`
}

type ReviewBalanceRequest struct {
	TargetStatus constants.BalanceStatus `json:"target_status" binding:"required"`
	Version      uint                    `json:"version" binding:"required"`
	ReviewNote   string                  `json:"review_note" binding:"required,min=6,max=1000"`
}

type InvalidateBalanceRequest struct {
	Version uint   `json:"version" binding:"required"`
	Reason  string `json:"reason" binding:"required,min=6,max=1000"`
}

type RecalculateBalanceRequest struct {
	Version uint `json:"version" binding:"required"`
}

// RecalculationReason 描述一次边界证据完整性闸门置位的结构化原因。
type RecalculationReason struct {
	Code       string `json:"code"`
	EntityType string `json:"entity_type"`
	EntityID   uint   `json:"entity_id"`
	Detail     string `json:"detail"`
	DetectedAt string `json:"detected_at"`
}

func (r RecalculationReason) key() string {
	return r.Code + ":" + r.EntityType + ":" + strconv.FormatUint(uint64(r.EntityID), 10)
}

// MergeRecalculationReasons 在既有原因上追加新原因，按代码与实体去重，保留首次检出时间。
func MergeRecalculationReasons(existing []RecalculationReason, additions ...RecalculationReason) []RecalculationReason {
	seen := make(map[string]struct{}, len(existing)+len(additions))
	merged := make([]RecalculationReason, 0, len(existing)+len(additions))
	for _, reason := range existing {
		if reason.Code == "" {
			continue
		}
		if _, ok := seen[reason.key()]; ok {
			continue
		}
		seen[reason.key()] = struct{}{}
		merged = append(merged, reason)
	}
	for _, reason := range additions {
		if reason.Code == "" {
			continue
		}
		if _, ok := seen[reason.key()]; ok {
			continue
		}
		seen[reason.key()] = struct{}{}
		merged = append(merged, reason)
	}
	return merged
}

type UncertaintyComponent struct {
	Source         string  `json:"source"`
	EntityID       uint    `json:"entity_id"`
	MassKG         float64 `json:"mass_kg"`
	UncertaintyPct float64 `json:"uncertainty_pct"`
	AbsoluteKG     float64 `json:"absolute_kg"`
}

type UncertaintyBreakdown struct {
	BalanceRunID uint                     `json:"balance_run_id"`
	CombinedKG   float64                  `json:"combined_kg"`
	LowerKG      float64                  `json:"lower_kg"`
	UpperKG      float64                  `json:"upper_kg"`
	Relationship constants.DeviationLevel `json:"relationship"`
	Components   []UncertaintyComponent   `json:"components"`
}
