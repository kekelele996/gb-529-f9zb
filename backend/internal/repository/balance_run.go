package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"lng-boiloff-gas-balance/backend/internal/constants"
	"lng-boiloff-gas-balance/backend/internal/dto"
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
	db      *gorm.DB
	dialect string
}

func NewBalanceRepository(db *gorm.DB) *BalanceRepository {
	return &BalanceRepository{db: db, dialect: db.Dialector.Name()}
}

// OpenForTank 返回该储罐仍处于非终态、可被闸门置为待重算的运行（含 calculating / pending_review / recalculate_required）。
func (r *BalanceRepository) OpenForTank(ctx context.Context, tx *gorm.DB, tankID uint) ([]model.BalanceRun, error) {
	var runs []model.BalanceRun
	err := tx.WithContext(ctx).
		Where("tank_id = ? AND balance_status IN ?", tankID, constants.BoundaryGateOpenStatuses).
		Order("id ASC").Find(&runs).Error
	if err != nil {
		return nil, fmt.Errorf("list open balance runs for gate: %w", err)
	}
	return runs, nil
}

// FlagForRecalculation 条件更新单条运行：状态仍为闸门开放状态且版本未变时，
// 合并待重算原因、置 recalculate_required。RowsAffected==0 表示已被并发改动，调用方跳过。
func (r *BalanceRepository) FlagForRecalculation(ctx context.Context, tx *gorm.DB, id, expectedVersion uint, reasons []dto.RecalculationReason) (bool, error) {
	if len(reasons) == 0 {
		return false, nil
	}
	raw, err := json.Marshal(reasons)
	if err != nil {
		return false, fmt.Errorf("marshal recalculation reasons: %w", err)
	}
	result := tx.WithContext(ctx).Model(&model.BalanceRun{}).
		Where("id = ? AND version = ? AND balance_status IN ?", id, expectedVersion, constants.BoundaryGateOpenStatuses).
		Updates(map[string]any{
			"balance_status":            constants.BalanceRecalculateRequired,
			"recalculation_reason_json": datatypes.JSON(raw),
			"version":                   gorm.Expr("version + 1"),
		})
	if result.Error != nil {
		return false, fmt.Errorf("flag balance run for recalculation: %w", result.Error)
	}
	return result.RowsAffected == 1, nil
}

// RecalculateInput 是事务内复核员替代旧运行时需要持久化的新运行及其证据。
type RecalculateInput struct {
	NewRun        *model.BalanceRun
	ReasonsJSON   datatypes.JSON
	NewRunSummary map[string]any
}

// RecalculateOutput 回读替代后的旧记录与新记录。
type RecalculateOutput struct {
	Old model.BalanceRun
	New model.BalanceRun
}

// ReplaceRun 在单个事务中以新计算记录原子替代待重算的旧记录：
// 旧记录置 superseded（条件更新 + 行锁保证并发只有一次成功），新记录携带 supersedes_id 入 pending_review。
// compute 在事务内执行，基于同一事务快照重新选取边界证据并完成计算。
func (r *BalanceRepository) ReplaceRun(
	ctx context.Context,
	id, expectedVersion uint,
	actor Actor,
	compute func(tx *gorm.DB) (RecalculateInput, error),
) (RecalculateOutput, error) {
	var output RecalculateOutput
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var old model.BalanceRun
		query := tx.Model(&model.BalanceRun{}).Where("id = ?", id)
		if r.dialect == "postgres" {
			query = query.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		if err := query.First(&old).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return api.NewError(404, "BALANCE_NOT_FOUND", "质量平衡运行不存在")
			}
			return fmt.Errorf("load balance run for replacement: %w", err)
		}
		if old.BalanceStatus == constants.BalanceSuperseded {
			return api.NewError(409, "BALANCE_ALREADY_SUPERSEDED", "该运行已被其他替代请求处理，刷新后请查看新运行")
		}
		if old.Version != expectedVersion {
			return api.NewError(409, "BALANCE_VERSION_CONFLICT", "平衡运行版本已变化，请刷新后重试")
		}
		if old.BalanceStatus != constants.BalanceRecalculateRequired {
			return api.WithDetails(api.NewError(409, "BALANCE_NOT_RECALCULATE_REQUIRED", "只有待重算的运行可以执行替代重算"), map[string]any{
				"current": old.BalanceStatus,
			})
		}

		input, err := compute(tx)
		if err != nil {
			return err
		}
		newRun := input.NewRun
		newRun.TankID = old.TankID
		newRun.PeriodStart = old.PeriodStart
		newRun.PeriodEnd = old.PeriodEnd
		newRun.BalanceStatus = constants.BalancePendingReview
		newRun.Version = 2
		newRun.CreatedBy = actor.UserID
		now := time.Now().UTC()
		newRun.RecalculatedAt = &now
		newRun.SupersedesID = &old.ID

		if err := tx.Create(newRun).Error; err != nil {
			if errors.Is(err, gorm.ErrDuplicatedKey) {
				return api.NewError(409, "BALANCE_ALREADY_SUPERSEDED", "该运行已被其他替代请求处理，刷新后请查看新运行")
			}
			return fmt.Errorf("create recalculated balance run: %w", err)
		}

		result := tx.Model(&model.BalanceRun{}).
			Where("id = ? AND version = ? AND balance_status = ? AND superseded_by_id IS NULL", id, expectedVersion, constants.BalanceRecalculateRequired).
			Updates(map[string]any{
				"balance_status":            constants.BalanceSuperseded,
				"version":                   gorm.Expr("version + 1"),
				"superseded_by_id":          newRun.ID,
				"recalculation_reason_json": input.ReasonsJSON,
			})
		if result.Error != nil {
			return fmt.Errorf("supersede original balance run: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return api.NewError(409, "BALANCE_ALREADY_SUPERSEDED", "该运行已被其他替代请求处理，刷新后请查看新运行")
		}

		if err := tx.First(&output.Old, id).Error; err != nil {
			return fmt.Errorf("reload superseded balance run: %w", err)
		}
		if err := tx.Preload("Tank").First(&output.New, newRun.ID).Error; err != nil {
			return fmt.Errorf("reload recalculated balance run: %w", err)
		}

		recalculatedAudit := NewAudit(actor, "balance_run.recalculated", "balance_run", newRun.ID, map[string]any{
			"superseded_run_id": old.ID,
		}, map[string]any{
			"record":        newRun,
			"supersedes_id": old.ID,
			"reason":        input.NewRunSummary,
		})
		if err := tx.Create(&recalculatedAudit).Error; err != nil {
			return fmt.Errorf("audit recalculated balance run: %w", err)
		}
		supersededAudit := NewAudit(actor, "balance_run.superseded", "balance_run", old.ID, map[string]any{
			"record": old,
		}, map[string]any{
			"record":              output.Old,
			"recalculated_run_id": newRun.ID,
		})
		if err := tx.Create(&supersededAudit).Error; err != nil {
			return fmt.Errorf("audit superseded balance run: %w", err)
		}
		return nil
	})
	return output, err
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
