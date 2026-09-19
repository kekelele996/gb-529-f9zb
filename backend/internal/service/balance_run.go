package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"

	"lng-boiloff-gas-balance/backend/internal/balance"
	"lng-boiloff-gas-balance/backend/internal/constants"
	"lng-boiloff-gas-balance/backend/internal/dto"
	"lng-boiloff-gas-balance/backend/internal/model"
	"lng-boiloff-gas-balance/backend/internal/repository"
	"lng-boiloff-gas-balance/backend/pkg/api"
)

const balanceAlgorithmVersion = "mass-balance-v1.0"

// BoundaryEvidenceGate 是边界证据完整性闸门：证据写入事务内把受影响的非终态运行置为待重算。
// 由 BalanceService 实现，注入计量快照、物理转移与储罐参数的写入路径。
type BoundaryEvidenceGate interface {
	EnforceMeasurement(ctx context.Context, tx *gorm.DB, actor repository.Actor, snapshot model.MeasurementSnapshot) error
	EnforceTransfer(ctx context.Context, tx *gorm.DB, actor repository.Actor, transfer model.TransferOperation) error
	EnforceCoefficients(ctx context.Context, tx *gorm.DB, actor repository.Actor, before, after model.StorageTank) error
}

type BalanceService struct {
	repo            *repository.BalanceRepository
	tankRepo        *repository.TankRepository
	measurementRepo *repository.MeasurementRepository
	transferRepo    *repository.TransferRepository
}

func NewBalanceService(repo *repository.BalanceRepository, tankRepo *repository.TankRepository, measurementRepo *repository.MeasurementRepository, transferRepo *repository.TransferRepository) *BalanceService {
	return &BalanceService{repo: repo, tankRepo: tankRepo, measurementRepo: measurementRepo, transferRepo: transferRepo}
}

func (s *BalanceService) List(ctx context.Context, filter repository.BalanceFilter) ([]model.BalanceRun, int64, error) {
	if filter.Status != "" && !constants.ValidBalanceStatus(constants.BalanceStatus(filter.Status)) {
		return nil, 0, api.NewError(400, "INVALID_BALANCE_STATUS", "平衡状态筛选值无效")
	}
	return s.repo.List(ctx, filter)
}

func (s *BalanceService) Get(ctx context.Context, id uint) (model.BalanceRun, error) {
	return s.repo.Get(ctx, id)
}

func (s *BalanceService) Run(ctx context.Context, request dto.RunBalanceRequest, actor repository.Actor) (model.BalanceRun, error) {
	if !constants.CanAnalyze(actor.Role) {
		return model.BalanceRun{}, api.ErrForbidden
	}
	if request.PeriodStart == nil || request.PeriodEnd == nil {
		return model.BalanceRun{}, api.NewError(400, "BALANCE_PERIOD_REQUIRED", "必须提供平衡期间起止时间")
	}
	start, end := request.PeriodStart.UTC(), request.PeriodEnd.UTC()
	if !end.After(start) {
		return model.BalanceRun{}, api.NewError(422, "INVALID_BALANCE_PERIOD", "平衡期间结束时间必须晚于开始时间")
	}
	if end.Sub(start) > 90*24*time.Hour {
		return model.BalanceRun{}, api.NewError(422, "BALANCE_PERIOD_TOO_LONG", "单次质量平衡期间不能超过 90 天")
	}
	tank, err := s.tankRepo.Get(ctx, request.TankID)
	if err != nil {
		return model.BalanceRun{}, err
	}
	if tank.TankStatus != "active" {
		return model.BalanceRun{}, api.NewError(409, "TANK_NOT_ACTIVE", "只有启用储罐可以运行质量平衡")
	}
	opening, closing, err := s.measurementRepo.BoundarySnapshots(ctx, tank.ID, start, end)
	if err != nil {
		return model.BalanceRun{}, err
	}
	transfers, err := s.transferRepo.ConfirmedForPeriod(ctx, tank.ID, start, end)
	if err != nil {
		return model.BalanceRun{}, err
	}
	calculation, snapshotJSON, evidenceJSON, err := calculateBalanceRun(tank, opening, closing, transfers, start, end)
	if err != nil {
		return model.BalanceRun{}, err
	}
	run := model.BalanceRun{
		TankID:             tank.ID,
		PeriodStart:        start,
		PeriodEnd:          end,
		BalanceStatus:      constants.BalanceCalculating,
		InputSnapshotJSON:  datatypes.JSON(snapshotJSON),
		EvidenceJSON:       datatypes.JSON(evidenceJSON),
		OpeningMassKG:      calculation.OpeningMassKG,
		ClosingMassKG:      calculation.ClosingMassKG,
		NetTransferKG:      calculation.NetTransferKG,
		EstimatedBOGKG:     calculation.EstimatedBOGKG,
		UncertaintyKG:      calculation.UncertaintyKG,
		IntervalLowerKG:    calculation.IntervalLowerKG,
		IntervalUpperKG:    calculation.IntervalUpperKG,
		DeviationPct:       calculation.DeviationPct,
		DeviationLevel:     calculation.DeviationLevel,
		CoefficientVersion: tank.CoefficientVersion,
		Version:            2,
		CreatedBy:          actor.UserID,
	}
	if err := s.repo.CreateCalculated(ctx, &run, actor); err != nil {
		return model.BalanceRun{}, err
	}
	run.Tank = &tank
	return run, nil
}

type calculatedBalance struct {
	OpeningMassKG   float64
	ClosingMassKG   float64
	NetTransferKG   float64
	EstimatedBOGKG  float64
	UncertaintyKG   float64
	IntervalLowerKG float64
	IntervalUpperKG float64
	DeviationPct    float64
	DeviationLevel  constants.DeviationLevel
}

type balanceEvidence struct {
	AlgorithmVersion string                   `json:"algorithm_version"`
	Equation         map[string]float64       `json:"equation"`
	Uncertainty      dto.UncertaintyBreakdown `json:"uncertainty"`
	SafetyBoundary   string                   `json:"safety_boundary"`
}

func calculateBalanceRun(tank model.StorageTank, opening, closing model.MeasurementSnapshot, transfers []model.TransferOperation, start, end time.Time) (calculatedBalance, []byte, []byte, error) {
	inflows, outflows := make([]float64, 0), make([]float64, 0)
	uncertaintyInputs := []balance.UncertaintyInput{
		{Source: "opening_snapshot", EntityID: opening.ID, MassKG: opening.CalculatedLiquidMassKG, UncertaintyPct: opening.MeasurementUncertaintyPct},
		{Source: "closing_snapshot", EntityID: closing.ID, MassKG: closing.CalculatedLiquidMassKG, UncertaintyPct: closing.MeasurementUncertaintyPct},
	}
	components := []dto.UncertaintyComponent{
		{Source: "opening_snapshot", EntityID: opening.ID, MassKG: opening.CalculatedLiquidMassKG, UncertaintyPct: opening.MeasurementUncertaintyPct},
		{Source: "closing_snapshot", EntityID: closing.ID, MassKG: closing.CalculatedLiquidMassKG, UncertaintyPct: closing.MeasurementUncertaintyPct},
	}
	for _, transfer := range transfers {
		if transfer.OperationType == "inflow" {
			inflows = append(inflows, transfer.MeasuredMassKG)
		} else {
			outflows = append(outflows, transfer.MeasuredMassKG)
		}
		source := "transfer_" + transfer.OperationType
		uncertaintyInputs = append(uncertaintyInputs, balance.UncertaintyInput{
			Source: source, EntityID: transfer.ID, MassKG: transfer.MeasuredMassKG, UncertaintyPct: transfer.MeasurementUncertaintyPct,
		})
		components = append(components, dto.UncertaintyComponent{
			Source: source, EntityID: transfer.ID, MassKG: transfer.MeasuredMassKG, UncertaintyPct: transfer.MeasurementUncertaintyPct,
		})
	}
	net, err := balance.NetTransfer(inflows, outflows)
	if err != nil {
		return calculatedBalance{}, nil, nil, fmt.Errorf("calculate net transfer: %w", err)
	}
	deviation, err := balance.PhysicalBalance(opening.CalculatedLiquidMassKG, net, closing.CalculatedLiquidMassKG)
	if err != nil {
		return calculatedBalance{}, nil, nil, fmt.Errorf("calculate physical mass balance: %w", err)
	}
	propagated, err := balance.PropagateUncertainty(uncertaintyInputs)
	if err != nil {
		return calculatedBalance{}, nil, nil, fmt.Errorf("propagate measurement uncertainty: %w", err)
	}
	for index := range components {
		components[index].AbsoluteKG = propagated.Components[index]
	}
	valid := opening.QualityFlag != constants.QualityInvalid && closing.QualityFlag != constants.QualityInvalid
	level := balance.ClassifyDeviation(deviation, propagated.CombinedKG, valid)
	lower, upper := balance.ConfidenceInterval(deviation, propagated.CombinedKG)
	breakdown := dto.UncertaintyBreakdown{
		CombinedKG:   propagated.CombinedKG,
		LowerKG:      lower,
		UpperKG:      upper,
		Relationship: level,
		Components:   components,
	}
	evidence := balanceEvidence{
		AlgorithmVersion: balanceAlgorithmVersion,
		Equation: map[string]float64{
			"opening_mass_kg":                  opening.CalculatedLiquidMassKG,
			"net_transfer_kg":                  net,
			"closing_mass_kg":                  closing.CalculatedLiquidMassKG,
			"estimated_bog_and_unexplained_kg": deviation,
		},
		Uncertainty:    breakdown,
		SafetyBoundary: "未解释差异仅为工程分析结果，不直接认定为泄漏或安全事件。",
	}
	inputSnapshot := map[string]any{
		"algorithm_version":   balanceAlgorithmVersion,
		"coefficient_version": tank.CoefficientVersion,
		"period_start":        start,
		"period_end":          end,
		"tank":                tank,
		"opening_snapshot":    opening,
		"closing_snapshot":    closing,
		"confirmed_transfers": transfers,
	}
	snapshotJSON, err := json.Marshal(inputSnapshot)
	if err != nil {
		return calculatedBalance{}, nil, nil, fmt.Errorf("marshal immutable balance input snapshot: %w", err)
	}
	evidenceJSON, err := json.Marshal(evidence)
	if err != nil {
		return calculatedBalance{}, nil, nil, fmt.Errorf("marshal balance evidence: %w", err)
	}
	return calculatedBalance{
		OpeningMassKG:   opening.CalculatedLiquidMassKG,
		ClosingMassKG:   closing.CalculatedLiquidMassKG,
		NetTransferKG:   net,
		EstimatedBOGKG:  deviation,
		UncertaintyKG:   propagated.CombinedKG,
		IntervalLowerKG: lower,
		IntervalUpperKG: upper,
		DeviationPct:    balance.DeviationPercent(deviation, opening.CalculatedLiquidMassKG),
		DeviationLevel:  level,
	}, snapshotJSON, evidenceJSON, nil
}

func (s *BalanceService) Submit(ctx context.Context, id uint, request dto.SubmitBalanceRequest, actor repository.Actor) (model.BalanceRun, error) {
	if !constants.CanAnalyze(actor.Role) {
		return model.BalanceRun{}, api.ErrForbidden
	}
	current, err := s.repo.Get(ctx, id)
	if err != nil {
		return model.BalanceRun{}, err
	}
	if current.BalanceStatus == constants.BalanceRecalculateRequired {
		return model.BalanceRun{}, api.NewError(409, "BALANCE_RECALCULATE_REQUIRED", "边界证据已更新，该运行待重算，不能提交复核，请等待复核员替代重算")
	}
	return s.repo.Transition(ctx, id, request.Version, constants.BalancePendingReview, "提交独立复核", nil, actor)
}

func (s *BalanceService) Review(ctx context.Context, id uint, request dto.ReviewBalanceRequest, actor repository.Actor) (model.BalanceRun, error) {
	if !constants.CanReview(actor.Role) {
		return model.BalanceRun{}, api.ErrForbidden
	}
	if request.TargetStatus != constants.BalanceAccepted && request.TargetStatus != constants.BalanceRejected {
		return model.BalanceRun{}, api.NewError(422, "INVALID_REVIEW_DECISION", "复核目标状态只能是 accepted 或 rejected")
	}
	current, err := s.repo.Get(ctx, id)
	if err != nil {
		return model.BalanceRun{}, err
	}
	if request.TargetStatus == constants.BalanceAccepted && current.BalanceStatus == constants.BalanceRecalculateRequired {
		return model.BalanceRun{}, api.NewError(409, "BALANCE_RECALCULATE_REQUIRED", "边界证据完整性闸门未通过：存在更接近边界的新证据，待重算运行不能接受，请先执行替代重算")
	}
	note := strings.TrimSpace(request.ReviewNote)
	return s.repo.Transition(ctx, id, request.Version, request.TargetStatus, note, &actor.UserID, actor)
}

func (s *BalanceService) Invalidate(ctx context.Context, id uint, request dto.InvalidateBalanceRequest, actor repository.Actor) (model.BalanceRun, error) {
	if !constants.CanAdmin(actor.Role) {
		return model.BalanceRun{}, api.ErrForbidden
	}
	note := strings.TrimSpace(request.Reason)
	return s.repo.Transition(ctx, id, request.Version, constants.BalanceInvalidated, note, &actor.UserID, actor)
}

// Recalculate 由复核员对闸门置位的待重算运行执行原子替代：事务内重新选取边界证据、
// 重算并生成 pending_review 新记录，同时把旧记录置为 superseded。任一步失败整体回滚。
func (s *BalanceService) Recalculate(ctx context.Context, id uint, request dto.RecalculateBalanceRequest, actor repository.Actor) (repository.RecalculateOutput, error) {
	if !constants.CanReview(actor.Role) {
		return repository.RecalculateOutput{}, api.ErrForbidden
	}
	output, err := s.repo.ReplaceRun(ctx, id, request.Version, actor, func(tx *gorm.DB) (repository.RecalculateInput, error) {
		var old model.BalanceRun
		if err := tx.First(&old, id).Error; err != nil {
			return repository.RecalculateInput{}, fmt.Errorf("reload run inside replacement tx: %w", err)
		}
		tank, err := s.tankRepo.GetTx(ctx, tx, old.TankID)
		if err != nil {
			return repository.RecalculateInput{}, err
		}
		opening, closing, err := s.measurementRepo.BoundarySnapshotsTx(ctx, tx, old.TankID, old.PeriodStart, old.PeriodEnd)
		if err != nil {
			return repository.RecalculateInput{}, err
		}
		transfers, err := s.transferRepo.ConfirmedForPeriodTx(ctx, tx, old.TankID, old.PeriodStart, old.PeriodEnd)
		if err != nil {
			return repository.RecalculateInput{}, err
		}
		calculation, snapshotJSON, evidenceJSON, err := calculateBalanceRun(tank, opening, closing, transfers, old.PeriodStart, old.PeriodEnd)
		if err != nil {
			return repository.RecalculateInput{}, err
		}
		newRun := model.BalanceRun{
			InputSnapshotJSON:  datatypes.JSON(snapshotJSON),
			EvidenceJSON:       datatypes.JSON(evidenceJSON),
			OpeningMassKG:      calculation.OpeningMassKG,
			ClosingMassKG:      calculation.ClosingMassKG,
			NetTransferKG:      calculation.NetTransferKG,
			EstimatedBOGKG:     calculation.EstimatedBOGKG,
			UncertaintyKG:      calculation.UncertaintyKG,
			IntervalLowerKG:    calculation.IntervalLowerKG,
			IntervalUpperKG:    calculation.IntervalUpperKG,
			DeviationPct:       calculation.DeviationPct,
			DeviationLevel:     calculation.DeviationLevel,
			CoefficientVersion: tank.CoefficientVersion,
		}
		var reasons []dto.RecalculationReason
		if len(old.RecalculationReason) > 0 {
			if err := json.Unmarshal(old.RecalculationReason, &reasons); err != nil {
				return repository.RecalculateInput{}, fmt.Errorf("decode recalculation reasons for replacement: %w", err)
			}
		}
		reasonsJSON := datatypes.JSON([]byte("[]"))
		if len(reasons) > 0 {
			reasonsJSON = datatypes.JSON(old.RecalculationReason)
		}
		return repository.RecalculateInput{
			NewRun:      &newRun,
			ReasonsJSON: reasonsJSON,
			NewRunSummary: map[string]any{
				"estimated_bog_kg":     calculation.EstimatedBOGKG,
				"uncertainty_kg":       calculation.UncertaintyKG,
				"deviation_level":      calculation.DeviationLevel,
				"coefficient_version":  tank.CoefficientVersion,
				"recalculation_reason": reasons,
			},
		}, nil
	})
	if err != nil {
		return repository.RecalculateOutput{}, err
	}
	return output, nil
}

func (s *BalanceService) Uncertainty(ctx context.Context, id uint) (dto.UncertaintyBreakdown, error) {
	run, err := s.repo.Get(ctx, id)
	if err != nil {
		return dto.UncertaintyBreakdown{}, err
	}
	var evidence balanceEvidence
	if err := json.Unmarshal(run.EvidenceJSON, &evidence); err != nil {
		return dto.UncertaintyBreakdown{}, fmt.Errorf("decode stored uncertainty evidence: %w", err)
	}
	if math.Abs(evidence.Uncertainty.CombinedKG-run.UncertaintyKG) > 0.01 {
		return dto.UncertaintyBreakdown{}, api.NewError(500, "EVIDENCE_INTEGRITY_ERROR", "存储的不确定度证据与运行结果不一致")
	}
	evidence.Uncertainty.BalanceRunID = run.ID
	return evidence.Uncertainty, nil
}

type storedBalanceInputs struct {
	CoefficientVersion string                    `json:"coefficient_version"`
	Opening            model.MeasurementSnapshot `json:"opening_snapshot"`
	Closing            model.MeasurementSnapshot `json:"closing_snapshot"`
	ConfirmedTransfers []model.TransferOperation `json:"confirmed_transfers"`
}

func decodeStoredBalanceInputs(run model.BalanceRun) (storedBalanceInputs, error) {
	var stored storedBalanceInputs
	if len(run.InputSnapshotJSON) == 0 {
		return stored, fmt.Errorf("balance run %d missing stored input snapshot", run.ID)
	}
	if err := json.Unmarshal(run.InputSnapshotJSON, &stored); err != nil {
		return stored, fmt.Errorf("decode balance run %d input snapshot: %w", run.ID, err)
	}
	return stored, nil
}

func decodeExistingReasons(raw datatypes.JSON) []dto.RecalculationReason {
	if len(raw) == 0 {
		return nil
	}
	var reasons []dto.RecalculationReason
	if err := json.Unmarshal(raw, &reasons); err != nil {
		return nil
	}
	return reasons
}

func newRecalculationReason(code, entityType string, entityID uint, detail string) dto.RecalculationReason {
	return dto.RecalculationReason{
		Code:       code,
		EntityType: entityType,
		EntityID:   entityID,
		Detail:     detail,
		DetectedAt: time.Now().UTC().Format(time.RFC3339),
	}
}

// EnforceMeasurement 处理补录更接近边界的有效计量快照。
func (s *BalanceService) EnforceMeasurement(ctx context.Context, tx *gorm.DB, actor repository.Actor, snapshot model.MeasurementSnapshot) error {
	if snapshot.QualityFlag == constants.QualityInvalid {
		return nil
	}
	return s.applyGate(ctx, tx, snapshot.TankID, actor, func(run model.BalanceRun, stored storedBalanceInputs) ([]dto.RecalculationReason, error) {
		opening, closing, err := s.measurementRepo.BoundarySnapshotsTx(ctx, tx, run.TankID, run.PeriodStart, run.PeriodEnd)
		if err != nil {
			var appErr *api.Error
			if errors.As(err, &appErr) {
				return nil, nil // 边界尚缺失时不置位，保留原有的缺失边界校验
			}
			return nil, err
		}
		reasons := make([]dto.RecalculationReason, 0, 2)
		if opening.ID != stored.Opening.ID {
			reasons = append(reasons, newRecalculationReason(
				constants.RecalcReasonBoundarySnapshotCloser, "measurement_snapshot", opening.ID,
				fmt.Sprintf("补录的有效计量快照 #%d（%s）比原期初证据 #%d 更接近期初边界", opening.ID, opening.MeasuredAt.UTC().Format(time.RFC3339), stored.Opening.ID),
			))
		}
		if closing.ID != stored.Closing.ID {
			reasons = append(reasons, newRecalculationReason(
				constants.RecalcReasonBoundarySnapshotCloser, "measurement_snapshot", closing.ID,
				fmt.Sprintf("补录的有效计量快照 #%d（%s）比原期末证据 #%d 更接近期末边界", closing.ID, closing.MeasuredAt.UTC().Format(time.RFC3339), stored.Closing.ID),
			))
		}
		return reasons, nil
	})
}

// EnforceTransfer 处理晚录后确认、落入运行期间的流入/流出。
func (s *BalanceService) EnforceTransfer(ctx context.Context, tx *gorm.DB, actor repository.Actor, transfer model.TransferOperation) error {
	return s.applyGate(ctx, tx, transfer.TankID, actor, func(run model.BalanceRun, stored storedBalanceInputs) ([]dto.RecalculationReason, error) {
		withinPeriod := !transfer.StartAt.UTC().Before(run.PeriodStart.UTC()) && !transfer.EndAt.UTC().After(run.PeriodEnd.UTC())
		if !withinPeriod {
			return nil, nil
		}
		for _, included := range stored.ConfirmedTransfers {
			if included.ID == transfer.ID {
				return nil, nil
			}
		}
		direction := "流入"
		if transfer.OperationType == "outflow" {
			direction = "流出"
		}
		return []dto.RecalculationReason{newRecalculationReason(
			constants.RecalcReasonLateTransferConfirmed, "transfer_operation", transfer.ID,
			fmt.Sprintf("晚录确认的%s #%d（%s 至 %s，%.0f kg）未包含在原运行证据中",
				direction, transfer.ID,
				transfer.StartAt.UTC().Format(time.RFC3339), transfer.EndAt.UTC().Format(time.RFC3339),
				transfer.MeasuredMassKG),
		)}, nil
	})
}

// EnforceCoefficients 处理罐容系数版本更新。
func (s *BalanceService) EnforceCoefficients(ctx context.Context, tx *gorm.DB, actor repository.Actor, before, after model.StorageTank) error {
	return s.applyGate(ctx, tx, after.ID, actor, func(run model.BalanceRun, stored storedBalanceInputs) ([]dto.RecalculationReason, error) {
		if stored.CoefficientVersion == after.CoefficientVersion {
			return nil, nil
		}
		return []dto.RecalculationReason{newRecalculationReason(
			constants.RecalcReasonCoefficientVersion, "storage_tank", after.ID,
			fmt.Sprintf("罐容系数版本由 %s 更新为 %s，原运行固化的系数证据已过期",
				stored.CoefficientVersion, after.CoefficientVersion),
		)}, nil
	})
}

func (s *BalanceService) applyGate(
	ctx context.Context,
	tx *gorm.DB,
	tankID uint,
	actor repository.Actor,
	derive func(run model.BalanceRun, stored storedBalanceInputs) ([]dto.RecalculationReason, error),
) error {
	runs, err := s.repo.OpenForTank(ctx, tx, tankID)
	if err != nil {
		return err
	}
	for _, run := range runs {
		stored, err := decodeStoredBalanceInputs(run)
		if err != nil {
			return err
		}
		additions, err := derive(run, stored)
		if err != nil {
			return err
		}
		if len(additions) == 0 {
			continue
		}
		merged := dto.MergeRecalculationReasons(decodeExistingReasons(run.RecalculationReason), additions...)
		before := run
		flagged, err := s.repo.FlagForRecalculation(ctx, tx, run.ID, run.Version, merged)
		if err != nil {
			return err
		}
		if !flagged {
			continue
		}
		var after model.BalanceRun
		if err := tx.First(&after, run.ID).Error; err != nil {
			return fmt.Errorf("reload flagged balance run: %w", err)
		}
		audit := repository.NewAudit(actor, "balance_run.recalculate_required", "balance_run", run.ID,
			map[string]any{"record": before}, map[string]any{"record": after, "reasons": merged})
		if err := tx.Create(&audit).Error; err != nil {
			return fmt.Errorf("audit recalculation gate flag: %w", err)
		}
	}
	return nil
}
