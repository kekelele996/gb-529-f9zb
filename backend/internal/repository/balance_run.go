package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/gorm"

	"lng-boiloff-gas-balance/backend/internal/constants"
	"lng-boiloff-gas-balance/backend/internal/model"
	"lng-boiloff-gas-balance/backend/pkg/api"
)

type BalanceFilter struct {
	TankID   uint
	Status   string
	Page     int
	PageSize int
}

type BalanceRepository struct {
	db *gorm.DB
}

func NewBalanceRepository(db *gorm.DB) *BalanceRepository {
	return &BalanceRepository{db: db}
}

func (r *BalanceRepository) List(ctx context.Context, filter BalanceFilter) ([]model.BalanceRun, int64, error) {
	filter.Page, filter.PageSize = normalizePage(filter.Page, filter.PageSize)
	query := r.db.WithContext(ctx).Model(&model.BalanceRun{})
	if filter.TankID > 0 {
		query = query.Where("tank_id = ?", filter.TankID)
	}
	if filter.Status != "" {
		query = query.Where("balance_status = ?", filter.Status)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("count balance runs: %w", err)
	}
	var runs []model.BalanceRun
	if err := query.Preload("Tank").Order("created_at DESC, id DESC").
		Offset((filter.Page - 1) * filter.PageSize).Limit(filter.PageSize).Find(&runs).Error; err != nil {
		return nil, 0, fmt.Errorf("list balance runs: %w", err)
	}
	return runs, total, nil
}

func (r *BalanceRepository) Get(ctx context.Context, id uint) (model.BalanceRun, error) {
	var run model.BalanceRun
	if err := r.db.WithContext(ctx).Preload("Tank").First(&run, id).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return model.BalanceRun{}, api.NewError(404, "BALANCE_NOT_FOUND", "质量平衡运行不存在")
		}
		return model.BalanceRun{}, fmt.Errorf("get balance run: %w", err)
	}
	return run, nil
}

// ActiveSuccessor 返回替代旧运行且仍处于活跃（非驳回/作废/已替代）状态的后继。
func (r *BalanceRepository) ActiveSuccessor(ctx context.Context, predecessorID uint) (model.BalanceRun, bool, error) {
	var run model.BalanceRun
	err := r.db.WithContext(ctx).
		Where("supersedes_id = ? AND balance_status NOT IN ?", predecessorID,
			[]constants.BalanceStatus{constants.BalanceRejected, constants.BalanceInvalidated, constants.BalanceReplaced}).
		Order("id DESC").First(&run).Error
	if err == gorm.ErrRecordNotFound {
		return model.BalanceRun{}, false, nil
	}
	if err != nil {
		return model.BalanceRun{}, false, fmt.Errorf("load active successor: %w", err)
	}
	return run, true, nil
}

func (r *BalanceRepository) CreateCalculated(ctx context.Context, run *model.BalanceRun, actor Actor) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(run).Error; err != nil {
			return fmt.Errorf("create calculated balance run: %w", err)
		}
		queuedAudit := NewAudit(actor, "balance_run.queued", "balance_run", run.ID, nil, map[string]any{
			"tank_id": run.TankID, "period_start": run.PeriodStart, "period_end": run.PeriodEnd,
		})
		if err := tx.Create(&queuedAudit).Error; err != nil {
			return fmt.Errorf("audit queued balance run: %w", err)
		}
		calculatedAudit := NewAudit(actor, "balance_run.calculated", "balance_run", run.ID, map[string]any{"status": constants.BalanceQueued}, map[string]any{
			"status": run.BalanceStatus, "estimated_bog_kg": run.EstimatedBOGKG,
			"uncertainty_kg": run.UncertaintyKG, "deviation_level": run.DeviationLevel,
			"coefficient_version": run.CoefficientVersion,
		})
		if err := tx.Create(&calculatedAudit).Error; err != nil {
			return fmt.Errorf("audit balance calculation: %w", err)
		}
		return nil
	})
}

func (r *BalanceRepository) Transition(ctx context.Context, id, version uint, target constants.BalanceStatus, note string, reviewerID *uint, actor Actor) (model.BalanceRun, error) {
	var updated model.BalanceRun
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var before model.BalanceRun
		if err := tx.First(&before, id).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				return api.NewError(404, "BALANCE_NOT_FOUND", "质量平衡运行不存在")
			}
			return fmt.Errorf("load balance run for transition: %w", err)
		}
		if before.Version != version {
			return api.NewError(409, "BALANCE_VERSION_CONFLICT", "平衡运行版本已变化，请刷新后重试")
		}
		if !constants.CanTransitionBalance(before.BalanceStatus, target) {
			return api.WithDetails(api.NewError(409, "INVALID_BALANCE_TRANSITION", "当前平衡状态不允许目标迁移"), map[string]any{
				"current": before.BalanceStatus, "target": target,
			})
		}
		updates := map[string]any{
			"balance_status": target,
			"version":        gorm.Expr("version + 1"),
			"review_note":    note,
		}
		if reviewerID != nil {
			now := time.Now().UTC()
			updates["reviewed_by"] = *reviewerID
			updates["reviewed_at"] = now
		}
		result := tx.Model(&model.BalanceRun{}).
			Where("id = ? AND version = ? AND balance_status = ?", id, version, before.BalanceStatus).
			Updates(updates)
		if result.Error != nil {
			return fmt.Errorf("transition balance run: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return api.NewError(409, "BALANCE_VERSION_CONFLICT", "平衡运行被其他请求更新")
		}
		if err := tx.First(&updated, id).Error; err != nil {
			return fmt.Errorf("reload balance run: %w", err)
		}
		audit := NewAudit(actor, "balance_run."+string(target), "balance_run", id, before, updated)
		if err := tx.Create(&audit).Error; err != nil {
			return fmt.Errorf("audit balance transition: %w", err)
		}
		return nil
	})
	return updated, err
}

// CreateRecalculation 原子地为待重算运行申领替代资格并创建重算记录。
// 条件更新保证同一旧运行的并发重算只能成功一次；若已有活跃后继运行则拒绝。
// 任一步失败时事务回滚，旧运行、替代链与审计保持原样。
func (r *BalanceRepository) CreateRecalculation(ctx context.Context, predecessorID, expectedVersion uint, successor *model.BalanceRun, actor Actor) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var predecessor model.BalanceRun
		if err := tx.First(&predecessor, predecessorID).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				return api.NewError(404, "BALANCE_NOT_FOUND", "质量平衡运行不存在")
			}
			return fmt.Errorf("load predecessor balance run: %w", err)
		}
		if predecessor.BalanceStatus != constants.BalanceRecalculationRequired {
			return api.WithDetails(api.NewError(409, "RECALCULATION_NOT_REQUIRED", "只有待重算状态的运行可以发起重新计算"), map[string]any{
				"current": predecessor.BalanceStatus,
			})
		}
		if predecessor.Version != expectedVersion {
			return api.NewError(409, "BALANCE_VERSION_CONFLICT", "平衡运行版本已变化，请刷新后重试")
		}
		var activeSuccessor int64
		if err := tx.Model(&model.BalanceRun{}).
			Where("supersedes_id = ? AND balance_status NOT IN ?", predecessorID, []constants.BalanceStatus{constants.BalanceRejected, constants.BalanceInvalidated, constants.BalanceReplaced}).
			Count(&activeSuccessor).Error; err != nil {
			return fmt.Errorf("check existing recalculation successor: %w", err)
		}
		if activeSuccessor > 0 {
			return api.NewError(409, "RECALCULATION_ALREADY_EXISTS", "该运行已存在活跃的重算后继记录")
		}
		successor.SupersedesID = &predecessorID
		if err := tx.Create(successor).Error; err != nil {
			return fmt.Errorf("create recalculated balance run: %w", err)
		}
		claimed := tx.Model(&model.BalanceRun{}).
			Where("id = ? AND version = ? AND balance_status = ?",
				predecessorID, expectedVersion, constants.BalanceRecalculationRequired).
			Updates(map[string]any{
				"superseded_by_id": successor.ID,
				"version":          gorm.Expr("version + 1"),
			})
		if claimed.Error != nil {
			return fmt.Errorf("claim predecessor for recalculation: %w", claimed.Error)
		}
		if claimed.RowsAffected != 1 {
			return api.NewError(409, "BALANCE_VERSION_CONFLICT", "平衡运行被其他请求更新，请刷新后重试")
		}
		queuedAudit := NewAudit(actor, "balance_run.queued", "balance_run", successor.ID, nil, map[string]any{
			"tank_id": successor.TankID, "period_start": successor.PeriodStart, "period_end": successor.PeriodEnd,
			"supersedes_id": predecessorID,
		})
		if err := tx.Create(&queuedAudit).Error; err != nil {
			return fmt.Errorf("audit queued recalculation run: %w", err)
		}
		calculatedAudit := NewAudit(actor, "balance_run.calculated", "balance_run", successor.ID, nil, map[string]any{
			"status": successor.BalanceStatus, "estimated_bog_kg": successor.EstimatedBOGKG,
			"uncertainty_kg": successor.UncertaintyKG, "deviation_level": successor.DeviationLevel,
			"coefficient_version": successor.CoefficientVersion, "supersedes_id": predecessorID,
		})
		if err := tx.Create(&calculatedAudit).Error; err != nil {
			return fmt.Errorf("audit recalculation result: %w", err)
		}
		claimAudit := NewAudit(actor, "balance_run.recalculation_claimed", "balance_run", predecessorID,
			map[string]any{"status": constants.BalanceRecalculationRequired, "version": expectedVersion},
			map[string]any{"superseded_by_id": successor.ID, "version": expectedVersion + 1})
		if err := tx.Create(&claimAudit).Error; err != nil {
			return fmt.Errorf("audit recalculation claim: %w", err)
		}
		return nil
	})
}

// ReplaceByAcceptedSuccessor 由复核员原子替代：接受新记录，并把替代链上的待重算旧记录
// （含链式待重算祖先）全部置为 replaced。条件更新 + 单一事务保证并发或重复替代只能成功一次；
// 任一步失败时旧记录、替代链与审计均保持原样。返回替代后被接受的新记录。
func (r *BalanceRepository) ReplaceByAcceptedSuccessor(ctx context.Context, predecessorID, successorID, predecessorVersion, successorVersion uint, note string, reviewer Actor) (model.BalanceRun, error) {
	var accepted model.BalanceRun
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var predecessor model.BalanceRun
		if err := tx.First(&predecessor, predecessorID).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				return api.NewError(404, "BALANCE_NOT_FOUND", "质量平衡运行不存在")
			}
			return fmt.Errorf("load predecessor for replacement: %w", err)
		}
		if predecessor.BalanceStatus != constants.BalanceRecalculationRequired {
			return api.WithDetails(api.NewError(409, "REPLACEMENT_NOT_PENDING", "旧记录不是待重算状态，无法执行替代"), map[string]any{
				"current": predecessor.BalanceStatus,
			})
		}
		if predecessor.Version != predecessorVersion {
			return api.NewError(409, "BALANCE_VERSION_CONFLICT", "旧运行版本已变化，请刷新后重试")
		}
		var successor model.BalanceRun
		if err := tx.First(&successor, successorID).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				return api.NewError(404, "BALANCE_NOT_FOUND", "重算后的质量平衡运行不存在")
			}
			return fmt.Errorf("load successor for replacement: %w", err)
		}
		if successor.SupersedesID == nil || *successor.SupersedesID != predecessorID {
			return api.NewError(422, "REPLACEMENT_CHAIN_MISMATCH", "重算记录与旧运行的替代链不一致")
		}
		if successor.BalanceStatus != constants.BalancePendingReview {
			return api.WithDetails(api.NewError(409, "SUCCESSOR_NOT_REVIEWABLE", "重算记录必须处于待复核状态才能接受并替代"), map[string]any{
				"current": successor.BalanceStatus,
			})
		}
		if successor.Version != successorVersion {
			return api.NewError(409, "BALANCE_VERSION_CONFLICT", "重算记录版本已变化，请刷新后重试")
		}
		if predecessor.SupersededByID == nil || *predecessor.SupersededByID != successorID {
			return api.NewError(422, "REPLACEMENT_CHAIN_MISMATCH", "旧运行未指向该重算后继，替代链不一致")
		}
		// 沿替代链向上收集仍待重算的祖先，替代时一次性闭环。
		ancestors := make([]model.BalanceRun, 0)
		anchor := predecessor
		for anchor.SupersedesID != nil {
			var older model.BalanceRun
			if err := tx.First(&older, *anchor.SupersedesID).Error; err != nil {
				return fmt.Errorf("load replacement chain ancestor: %w", err)
			}
			if older.BalanceStatus != constants.BalanceRecalculationRequired || older.SupersededByID == nil || *older.SupersededByID != anchor.ID {
				return api.NewError(409, "REPLACEMENT_CHAIN_BROKEN", "替代链已断裂或被其他请求修改，请刷新后重试")
			}
			ancestors = append(ancestors, older)
			anchor = older
		}
		now := time.Now().UTC()
		successorResult := tx.Model(&model.BalanceRun{}).
			Where("id = ? AND version = ? AND balance_status = ?", successorID, successorVersion, constants.BalancePendingReview).
			Updates(map[string]any{
				"balance_status": constants.BalanceAccepted,
				"version":        gorm.Expr("version + 1"),
				"review_note":    note,
				"reviewed_by":    reviewer.UserID,
				"reviewed_at":    now,
			})
		if successorResult.Error != nil {
			return fmt.Errorf("accept successor during replacement: %w", successorResult.Error)
		}
		if successorResult.RowsAffected != 1 {
			return api.NewError(409, "BALANCE_VERSION_CONFLICT", "重算记录被其他请求更新，替代未执行")
		}
		closeAncestor := func(run model.BalanceRun) error {
			result := tx.Model(&model.BalanceRun{}).
				Where("id = ? AND balance_status = ? AND superseded_by_id IS NOT NULL", run.ID, constants.BalanceRecalculationRequired).
				Updates(map[string]any{
					"balance_status": constants.BalanceReplaced,
					"version":        gorm.Expr("version + 1"),
				})
			if result.Error != nil {
				return fmt.Errorf("close replaced run %d: %w", run.ID, result.Error)
			}
			if result.RowsAffected != 1 {
				return api.NewError(409, "REPLACEMENT_RACE_LOST", "替代链已被其他请求处理，请刷新后重试")
			}
			audit := NewAudit(reviewer, "balance_run.replaced", "balance_run", run.ID,
				map[string]any{"status": constants.BalanceRecalculationRequired, "version": run.Version},
				map[string]any{"status": constants.BalanceReplaced, "superseded_by_id": *run.SupersededByID})
			if err := tx.Create(&audit).Error; err != nil {
				return fmt.Errorf("audit replaced ancestor: %w", err)
			}
			return nil
		}
		// 先关闭最远祖先，再逐级闭环，最后直接旧记录。
		for index := len(ancestors) - 1; index >= 0; index-- {
			if err := closeAncestor(ancestors[index]); err != nil {
				return err
			}
		}
		if err := closeAncestor(predecessor); err != nil {
			return err
		}
		acceptAudit := NewAudit(reviewer, "balance_run.replacement_accepted", "balance_run", successorID,
			map[string]any{"status": constants.BalancePendingReview, "version": successorVersion, "supersedes_id": predecessorID},
			map[string]any{"status": constants.BalanceAccepted, "supersedes_id": predecessorID, "replaced_ids": appendReplacedIDs(predecessorID, ancestors)})
		if err := tx.Create(&acceptAudit).Error; err != nil {
			return fmt.Errorf("audit replacement acceptance: %w", err)
		}
		if err := tx.Preload("Tank").Preload("Supersedes").First(&accepted, successorID).Error; err != nil {
			return fmt.Errorf("reload accepted successor: %w", err)
		}
		return nil
	})
	return accepted, err
}

func appendReplacedIDs(predecessorID uint, ancestors []model.BalanceRun) []uint {
	ids := make([]uint, 0, len(ancestors)+1)
	ids = append(ids, predecessorID)
	for _, ancestor := range ancestors {
		ids = append(ids, ancestor.ID)
	}
	return ids
}

// 边界证据完整性闸门命中原因。闸门在证据写入事务内运行，
// 命中后把同储罐、同期间、仍处于开放状态的运行原子转为待重算。
const (
	GateReasonBoundarySnapshot = "boundary_snapshot_superseded"
	GateReasonLateTransfer     = "late_transfer_confirmed"
	GateReasonCoefficient      = "coefficient_version_updated"
)

// frozenBalanceInput 对应 BalanceRun.InputSnapshotJSON 中固化的证据指针。
// 外层键由 service 的 map 字面量指定，内层实体使用模型自身的小写 JSON 标签。
type frozenBalanceInput struct {
	OpeningSnapshot struct {
		ID         uint      `json:"id"`
		MeasuredAt time.Time `json:"measured_at"`
	} `json:"opening_snapshot"`
	ClosingSnapshot struct {
		ID         uint      `json:"id"`
		MeasuredAt time.Time `json:"measured_at"`
	} `json:"closing_snapshot"`
	ConfirmedTransfers []struct {
		ID uint `json:"id"`
	} `json:"confirmed_transfers"`
}

// activeGateStatuses 是 IN 子句绑定值。
func activeGateStatuses() []string {
	return []string{
		string(constants.BalanceCalculating),
		string(constants.BalancePendingReview),
		string(constants.BalanceRecalculationRequired),
	}
}

// GateSnapshotCreated 在新增有效计量快照的事务中检查闸门。
func GateSnapshotCreated(tx *gorm.DB, ctx context.Context, snapshot model.MeasurementSnapshot, actor Actor) error {
	if snapshot.QualityFlag == constants.QualityInvalid {
		return nil
	}
	var runs []model.BalanceRun
	if err := tx.WithContext(ctx).
		Where("tank_id = ? AND balance_status IN ?", snapshot.TankID, activeGateStatuses()).
		Find(&runs).Error; err != nil {
		return fmt.Errorf("load runs for snapshot gate: %w", err)
	}
	for index := range runs {
		frozen, ok := parseFrozenInput(runs[index].InputSnapshotJSON)
		if !ok {
			continue
		}
		openingChanged := !snapshot.MeasuredAt.After(runs[index].PeriodStart) &&
			snapshot.MeasuredAt.After(frozen.OpeningSnapshot.MeasuredAt)
		closingChanged := snapshot.MeasuredAt.After(runs[index].PeriodStart) &&
			!snapshot.MeasuredAt.After(runs[index].PeriodEnd) &&
			snapshot.MeasuredAt.After(frozen.ClosingSnapshot.MeasuredAt)
		if openingChanged || closingChanged {
			boundary := "closing"
			if openingChanged {
				boundary = "opening"
			}
			reason := model.RecalculationReason{
				Code:        GateReasonBoundarySnapshot,
				Message:     fmt.Sprintf("补录的计量快照 #%d 更接近%s边界，原证据集不再完整。", snapshot.ID, boundaryLabel(boundary)),
				EvidenceRef: fmt.Sprintf("measurement_snapshot:%d", snapshot.ID),
				DetectedAt:  time.Now().UTC().Format(time.RFC3339),
			}
			if err := flagRunForRecalculation(tx, &runs[index], reason, actor); err != nil {
				return err
			}
		}
	}
	return nil
}

// GateTransferConfirmed 在转移被确认（含直接以确认态登记）的事务中检查闸门。
func GateTransferConfirmed(tx *gorm.DB, ctx context.Context, transfer model.TransferOperation, actor Actor) error {
	if transfer.OperationStatus != "confirmed" {
		return nil
	}
	var runs []model.BalanceRun
	if err := tx.WithContext(ctx).
		Where("tank_id = ? AND balance_status IN ?", transfer.TankID, activeGateStatuses()).
		Find(&runs).Error; err != nil {
		return fmt.Errorf("load runs for transfer gate: %w", err)
	}
	for index := range runs {
		run := &runs[index]
		withinPeriod := !transfer.StartAt.Before(run.PeriodStart) && !transfer.EndAt.After(run.PeriodEnd)
		if !withinPeriod {
			continue
		}
		frozen, ok := parseFrozenInput(run.InputSnapshotJSON)
		if !ok {
			continue
		}
		// 运行时已固化的确认转移一定在冻结清单中；晚确认的草稿或晚录的新确认不在其中。
		alreadyIncluded := false
		for _, item := range frozen.ConfirmedTransfers {
			if item.ID == transfer.ID {
				alreadyIncluded = true
				break
			}
		}
		if alreadyIncluded {
			continue
		}
		direction := "流出"
		if transfer.OperationType == "inflow" {
			direction = "流入"
		}
		reason := model.RecalculationReason{
			Code:        GateReasonLateTransfer,
			Message:     fmt.Sprintf("晚录的%s物理转移 #%d 已确认并落入本期间，原汇总缺少该证据。", direction, transfer.ID),
			EvidenceRef: fmt.Sprintf("transfer_operation:%d", transfer.ID),
			DetectedAt:  time.Now().UTC().Format(time.RFC3339),
		}
		if err := flagRunForRecalculation(tx, run, reason, actor); err != nil {
			return err
		}
	}
	return nil
}

// GateCoefficientVersionUpdated 在罐容系数版本实际更新的事务中检查闸门。
func GateCoefficientVersionUpdated(tx *gorm.DB, ctx context.Context, tankID uint, previousVersion, newVersion string, actor Actor) error {
	if previousVersion == newVersion {
		return nil
	}
	var runs []model.BalanceRun
	if err := tx.WithContext(ctx).
		Where("tank_id = ? AND balance_status IN ? AND coefficient_version <> ?", tankID, activeGateStatuses(), newVersion).
		Find(&runs).Error; err != nil {
		return fmt.Errorf("load runs for coefficient gate: %w", err)
	}
	for index := range runs {
		reason := model.RecalculationReason{
			Code:        GateReasonCoefficient,
			Message:     fmt.Sprintf("罐容系数版本由 %s 更新为 %s，固化系数已过时。", previousVersion, newVersion),
			EvidenceRef: fmt.Sprintf("storage_tank:%d", tankID),
			DetectedAt:  time.Now().UTC().Format(time.RFC3339),
		}
		if err := flagRunForRecalculation(tx, &runs[index], reason, actor); err != nil {
			return err
		}
	}
	return nil
}

// flagRunForRecalculation 以版本守卫的条件更新把运行转为待重算并合并原因；
// 并发或重复触发时只有一次版本推进生效。
func flagRunForRecalculation(tx *gorm.DB, run *model.BalanceRun, reason model.RecalculationReason, actor Actor) error {
	existing := run.ParseRecalculationReasons()
	for _, item := range existing {
		if item.Code == reason.Code && item.EvidenceRef == reason.EvidenceRef {
			return nil
		}
	}
	merged := append(existing, reason)
	mergedJSON, err := json.Marshal(merged)
	if err != nil {
		return fmt.Errorf("marshal recalculation reasons: %w", err)
	}
	result := tx.Model(&model.BalanceRun{}).
		Where("id = ? AND version = ? AND balance_status IN ?", run.ID, run.Version, activeGateStatuses()).
		Updates(map[string]any{
			"balance_status":        constants.BalanceRecalculationRequired,
			"recalculation_reasons": string(mergedJSON),
			"version":               gorm.Expr("version + 1"),
		})
	if result.Error != nil {
		return fmt.Errorf("flag balance run %d for recalculation: %w", run.ID, result.Error)
	}
	if result.RowsAffected != 1 {
		return nil
	}
	audit := NewAudit(actor, "balance_run.recalculation_required", "balance_run", run.ID,
		map[string]any{"status": run.BalanceStatus, "version": run.Version},
		map[string]any{"status": constants.BalanceRecalculationRequired, "reason": reason})
	if err := tx.Create(&audit).Error; err != nil {
		return fmt.Errorf("audit recalculation gate hit: %w", err)
	}
	run.BalanceStatus = constants.BalanceRecalculationRequired
	run.RecalculationReasons = mergedJSON
	run.Version++
	return nil
}

func parseFrozenInput(raw []byte) (frozenBalanceInput, bool) {
	var frozen frozenBalanceInput
	if len(raw) == 0 {
		return frozen, false
	}
	if err := json.Unmarshal(raw, &frozen); err != nil {
		return frozenBalanceInput{}, false
	}
	if frozen.OpeningSnapshot.ID == 0 || frozen.ClosingSnapshot.ID == 0 {
		return frozenBalanceInput{}, false
	}
	return frozen, true
}

func boundaryLabel(boundary string) string {
	if boundary == "opening" {
		return "期初"
	}
	return "期末"
}
