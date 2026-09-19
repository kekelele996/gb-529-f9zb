package service

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"lng-boiloff-gas-balance/backend/internal/constants"
	"lng-boiloff-gas-balance/backend/internal/dto"
	"lng-boiloff-gas-balance/backend/internal/model"
	"lng-boiloff-gas-balance/backend/internal/repository"
)

type gateFixture struct {
	db        *gorm.DB
	balances  *BalanceService
	tanks     *TankService
	snapshots *MeasurementService
	transfers *TransferService
}

func newGateFixture(t *testing.T, suffix string) gateFixture {
	t.Helper()
	// 使用临时文件 + WAL，模拟生产 PostgreSQL 的写串行化（内存 shared-cache 会产生 SQLite 特有的表级锁）。
	dbPath := filepath.Join(t.TempDir(), fmt.Sprintf("gate-%s-%d.db", suffix, time.Now().UnixNano()))
	dsn := "file:" + dbPath + "?_busy_timeout=5000&_journal_mode=WAL"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	if err := db.AutoMigrate(
		&model.User{}, &model.StorageTank{}, &model.MeasurementSnapshot{},
		&model.TransferOperation{}, &model.BalanceRun{}, &model.AuditEvent{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	tankRepo := repository.NewTankRepository(db)
	measurementRepo := repository.NewMeasurementRepository(db)
	transferRepo := repository.NewTransferRepository(db)
	balanceRepo := repository.NewBalanceRepository(db)
	balances := NewBalanceService(balanceRepo, tankRepo, measurementRepo, transferRepo)
	return gateFixture{
		db:        db,
		balances:  balances,
		tanks:     NewTankService(tankRepo, balances),
		snapshots: NewMeasurementService(measurementRepo, tankRepo, balances),
		transfers: NewTransferService(transferRepo, tankRepo, balances),
	}
}

var fixtureSeq uint64

func analystActor() repository.Actor {
	return repository.Actor{UserID: 1, Email: "analyst@lng.local", Role: constants.RoleProcessAnalyst, RequestID: "req-analyst"}
}

func reviewerActor() repository.Actor {
	return repository.Actor{UserID: 2, Email: "reviewer@lng.local", Role: constants.RoleReviewer, RequestID: "req-reviewer"}
}

func createGateTank(t *testing.T, f gateFixture, code, coefficientVersion string) model.StorageTank {
	t.Helper()
	seq := atomic.AddUint64(&fixtureSeq, 1)
	tank, err := f.tanks.Create(context.Background(), dto.CreateTankRequest{
		TankCode:              fmt.Sprintf("%s-%d", code, seq),
		Name:                  "闸门验证储罐",
		NominalCapacityM3:     200000,
		MinLevelM:             0,
		MaxLevelM:             20,
		ReferenceDensityKGM3:  450,
		ReferenceTemperatureC: -160,
		ThermalExpansionPerC:  0.0012,
		CapacityCurve:         []float64{0, 10000},
		CoefficientVersion:    coefficientVersion,
		TankStatus:            "active",
	}, analystActor())
	if err != nil {
		t.Fatalf("create tank: %v", err)
	}
	return tank
}

func createGateSnapshot(t *testing.T, f gateFixture, tankID uint, measuredAt time.Time, level float64, quality constants.QualityFlag) model.MeasurementSnapshot {
	t.Helper()
	snapshot, err := f.snapshots.Create(context.Background(), dto.CreateMeasurementRequest{
		TankID:                    tankID,
		MeasuredAt:                &measuredAt,
		LiquidLevelM:              level,
		LiquidTempC:               -160,
		VaporPressureKPA:          112,
		DensityKGM3:               450,
		MeasurementUncertaintyPct: 0.35,
		QualityFlag:               string(quality),
		SourceNote:                "闸门测试计量快照",
	}, analystActor())
	if err != nil {
		t.Fatalf("create snapshot at %s: %v", measuredAt.Format(time.RFC3339), err)
	}
	return snapshot
}

func runGateBalance(t *testing.T, f gateFixture, tankID uint, start, end time.Time) model.BalanceRun {
	t.Helper()
	run, err := f.balances.Run(context.Background(), dto.RunBalanceRequest{
		TankID: tankID, PeriodStart: &start, PeriodEnd: &end,
	}, analystActor())
	if err != nil {
		t.Fatalf("run balance: %v", err)
	}
	return run
}

func gateReasons(t *testing.T, run model.BalanceRun) []dto.RecalculationReason {
	t.Helper()
	if len(run.RecalculationReason) == 0 {
		return nil
	}
	var reasons []dto.RecalculationReason
	if err := json.Unmarshal(run.RecalculationReason, &reasons); err != nil {
		t.Fatalf("decode reasons: %v", err)
	}
	return reasons
}

type snapshotReference struct {
	Opening struct {
		ID uint `json:"id"`
	} `json:"opening_snapshot"`
	Closing struct {
		ID uint `json:"id"`
	} `json:"closing_snapshot"`
}

func decodeSnapshotRefs(t *testing.T, run model.BalanceRun) snapshotReference {
	t.Helper()
	var ref snapshotReference
	if err := json.Unmarshal(run.InputSnapshotJSON, &ref); err != nil {
		t.Fatalf("decode input snapshot: %v", err)
	}
	return ref
}

func TestBoundaryGateFlagsCloserSnapshotsAndReviewerAtomicallyReplaces(t *testing.T) {
	f := newGateFixture(t, "snapshot")
	tank := createGateTank(t, f, "GATE-S", "cv-v1")
	start := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	oldOpening := createGateSnapshot(t, f, tank.ID, start.Add(-2*time.Hour), 10, constants.QualityGood)
	oldClosing := createGateSnapshot(t, f, tank.ID, end.Add(-2*time.Hour), 9.9, constants.QualityGood)
	run := runGateBalance(t, f, tank.ID, start, end)

	closerOpening := createGateSnapshot(t, f, tank.ID, start.Add(-30*time.Minute), 10.02, constants.QualityGood)
	run, err := f.balances.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("reload run: %v", err)
	}
	if run.BalanceStatus != constants.BalanceRecalculateRequired {
		t.Fatalf("status = %s, want recalculate_required", run.BalanceStatus)
	}
	if reasons := gateReasons(t, run); len(reasons) != 1 || reasons[0].Code != constants.RecalcReasonBoundarySnapshotCloser || reasons[0].EntityID != closerOpening.ID {
		t.Fatalf("unexpected reasons after opening backfill: %+v", reasons)
	}

	closerClosing := createGateSnapshot(t, f, tank.ID, end.Add(-15*time.Minute), 9.92, constants.QualitySuspect)
	run, err = f.balances.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("reload run: %v", err)
	}
	reasons := gateReasons(t, run)
	if len(reasons) != 2 {
		t.Fatalf("expected merged two reasons, got %+v", reasons)
	}

	// 复核员不能接受待重算运行。
	if _, err := f.balances.Review(context.Background(), run.ID, dto.ReviewBalanceRequest{
		TargetStatus: constants.BalanceAccepted, Version: run.Version, ReviewNote: "尝试接受待重算结果应当失败",
	}, reviewerActor()); err == nil {
		t.Fatal("reviewer must not accept a recalculate_required run")
	}
	// 分析员也不能重新提交。
	if _, err := f.balances.Submit(context.Background(), run.ID, dto.SubmitBalanceRequest{Version: run.Version}, analystActor()); err == nil {
		t.Fatal("analyst must not resubmit a recalculate_required run")
	}

	// 非待重算状态不能调用替代。
	if _, err := f.balances.Recalculate(context.Background(), 99999, dto.RecalculateBalanceRequest{Version: 1}, reviewerActor()); err == nil {
		t.Fatal("recalculating a missing run must fail")
	}

	output, err := f.balances.Recalculate(context.Background(), run.ID, dto.RecalculateBalanceRequest{Version: run.Version}, reviewerActor())
	if err != nil {
		t.Fatalf("recalculate: %v", err)
	}
	if output.Old.BalanceStatus != constants.BalanceSuperseded || output.Old.SupersededByID == nil || *output.Old.SupersededByID != output.New.ID {
		t.Fatalf("old run chain incorrect: %+v", output.Old)
	}
	if output.New.BalanceStatus != constants.BalancePendingReview || output.New.SupersedesID == nil || *output.New.SupersedesID != output.Old.ID {
		t.Fatalf("new run chain incorrect: %+v", output.New)
	}
	refs := decodeSnapshotRefs(t, output.New)
	if refs.Opening.ID != closerOpening.ID || refs.Closing.ID != closerClosing.ID {
		t.Fatalf("recalculated run did not pick closer boundaries: %+v", refs)
	}
	if output.New.CoefficientVersion != "cv-v1" {
		t.Fatalf("unexpected coefficient version: %s", output.New.CoefficientVersion)
	}

	// 重复替代必须失败，且不得产生第二条后继。
	if _, err := f.balances.Recalculate(context.Background(), output.Old.ID, dto.RecalculateBalanceRequest{Version: output.Old.Version}, reviewerActor()); err == nil {
		t.Fatal("duplicate replacement must not succeed")
	}
	var successorCount int64
	if err := f.db.Model(&model.BalanceRun{}).Where("supersedes_id = ?", output.Old.ID).Count(&successorCount).Error; err != nil {
		t.Fatalf("count successors: %v", err)
	}
	if successorCount != 1 {
		t.Fatalf("successor count = %d, want 1", successorCount)
	}

	// 新记录可正常接受。
	accepted, err := f.balances.Review(context.Background(), output.New.ID, dto.ReviewBalanceRequest{
		TargetStatus: constants.BalanceAccepted, Version: output.New.Version, ReviewNote: "替代重算后证据完整，可以接受",
	}, reviewerActor())
	if err != nil {
		t.Fatalf("accept recalculated run: %v", err)
	}
	if accepted.BalanceStatus != constants.BalanceAccepted {
		t.Fatalf("status = %s, want accepted", accepted.BalanceStatus)
	}

	// 旧运行引用了被替代前的原始证据。
	oldRefs := decodeSnapshotRefs(t, output.Old)
	if oldRefs.Opening.ID != oldOpening.ID || oldRefs.Closing.ID != oldClosing.ID {
		t.Fatalf("superseded run evidence was mutated: %+v", oldRefs)
	}

	var auditCount int64
	if err := f.db.Model(&model.AuditEvent{}).
		Where("entity_type = ? AND action IN ?", "balance_run", []string{"balance_run.recalculate_required", "balance_run.recalculated", "balance_run.superseded"}).
		Count(&auditCount).Error; err != nil {
		t.Fatalf("count gate audits: %v", err)
	}
	if auditCount != 4 {
		t.Fatalf("gate audit count = %d, want 4", auditCount)
	}
}

func TestBoundaryGateIgnoresIrrelevantEvidence(t *testing.T) {
	f := newGateFixture(t, "ignore")
	tank := createGateTank(t, f, "GATE-I", "cv-v1")
	start := time.Date(2026, 6, 4, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	opening := createGateSnapshot(t, f, tank.ID, start.Add(-time.Hour), 10, constants.QualityGood)
	createGateSnapshot(t, f, tank.ID, end.Add(-time.Hour), 9.9, constants.QualityGood)
	run := runGateBalance(t, f, tank.ID, start, end)

	// 更早、不会改变边界选择的快照不置位。
	createGateSnapshot(t, f, tank.ID, start.Add(-3*time.Hour), 10.1, constants.QualityGood)
	reloaded, err := f.balances.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.BalanceStatus != constants.BalanceCalculating {
		t.Fatalf("status = %s, gate must ignore non-closer snapshot", reloaded.BalanceStatus)
	}

	// 无效快照不参与边界选择，也不置位。
	createGateSnapshot(t, f, tank.ID, start.Add(-15*time.Minute), 9.99, constants.QualityInvalid)
	reloaded, _ = f.balances.Get(context.Background(), run.ID)
	if reloaded.BalanceStatus != constants.BalanceCalculating {
		t.Fatalf("status = %s, invalid snapshots must not trigger gate", reloaded.BalanceStatus)
	}

	// 期间外的已确认转移不置位。
	outsideStart := end.Add(2 * time.Hour)
	outsideEnd := end.Add(3 * time.Hour)
	if _, err := f.transfers.Create(context.Background(), dto.CreateTransferRequest{
		TankID: tank.ID, OperationType: "inflow", StartAt: &outsideStart, EndAt: &outsideEnd,
		MeasuredMassKG: 12000, MeasurementUncertaintyPct: 0.3, CounterpartyRef: "OUTSIDE-PERIOD-1",
		OperationStatus: "confirmed",
	}, analystActor()); err != nil {
		t.Fatalf("create outside transfer: %v", err)
	}
	reloaded, _ = f.balances.Get(context.Background(), run.ID)
	if reloaded.BalanceStatus != constants.BalanceCalculating {
		t.Fatalf("status = %s, transfers outside the period must not trigger gate", reloaded.BalanceStatus)
	}
	if opening.ID == 0 {
		t.Fatal("opening snapshot missing")
	}
}

func TestBoundaryGateFlagsLateTransfersIncludingDraftConfirmation(t *testing.T) {
	f := newGateFixture(t, "transfer")
	tank := createGateTank(t, f, "GATE-T", "cv-v1")
	start := time.Date(2026, 6, 6, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	createGateSnapshot(t, f, tank.ID, start.Add(-time.Hour), 10, constants.QualityGood)
	createGateSnapshot(t, f, tank.ID, end.Add(-time.Hour), 9.9, constants.QualityGood)
	run := runGateBalance(t, f, tank.ID, start, end)

	// 晚录的草稿转移在确认瞬间置位。
	draftStart := start.Add(2 * time.Hour)
	draftEnd := start.Add(3 * time.Hour)
	draft, err := f.transfers.Create(context.Background(), dto.CreateTransferRequest{
		TankID: tank.ID, OperationType: "outflow", StartAt: &draftStart, EndAt: &draftEnd,
		MeasuredMassKG: 41000, MeasurementUncertaintyPct: 0.28, CounterpartyRef: "LATE-DRAFT-001",
		OperationStatus: "draft",
	}, analystActor())
	if err != nil {
		t.Fatalf("create draft transfer: %v", err)
	}
	reloaded, _ := f.balances.Get(context.Background(), run.ID)
	if reloaded.BalanceStatus != constants.BalanceCalculating {
		t.Fatalf("status = %s, draft transfer must not flag yet", reloaded.BalanceStatus)
	}
	confirmed, err := f.transfers.Transition(context.Background(), draft.ID, dto.TransitionTransferRequest{
		TargetStatus: "confirmed", Version: draft.Version, Reason: "班次后补录确认晚录流出",
	}, analystActor())
	if err != nil {
		t.Fatalf("confirm draft transfer: %v", err)
	}
	reloaded, _ = f.balances.Get(context.Background(), run.ID)
	if reloaded.BalanceStatus != constants.BalanceRecalculateRequired {
		t.Fatalf("status = %s, late confirmed transfer must flag run", reloaded.BalanceStatus)
	}
	if reasons := gateReasons(t, reloaded); len(reasons) != 1 || reasons[0].Code != constants.RecalcReasonLateTransferConfirmed || reasons[0].EntityID != confirmed.ID {
		t.Fatalf("unexpected late transfer reasons: %+v", reasons)
	}

	// 直接以 confirmed 晚录一条流入，同一条运行上合并原因。
	inStart := start.Add(5 * time.Hour)
	inEnd := start.Add(6 * time.Hour)
	if _, err := f.transfers.Create(context.Background(), dto.CreateTransferRequest{
		TankID: tank.ID, OperationType: "inflow", StartAt: &inStart, EndAt: &inEnd,
		MeasuredMassKG: 73000, MeasurementUncertaintyPct: 0.22, CounterpartyRef: "LATE-CONFIRMED-002",
		OperationStatus: "confirmed",
	}, analystActor()); err != nil {
		t.Fatalf("create confirmed late inflow: %v", err)
	}
	reloaded, _ = f.balances.Get(context.Background(), run.ID)
	if reasons := gateReasons(t, reloaded); len(reasons) != 2 {
		t.Fatalf("expected two merged transfer reasons, got %+v", reasons)
	}

	// 重算后的新运行必须包含两条新确认转移。
	output, err := f.balances.Recalculate(context.Background(), reloaded.ID, dto.RecalculateBalanceRequest{Version: reloaded.Version}, reviewerActor())
	if err != nil {
		t.Fatalf("recalculate after late transfers: %v", err)
	}
	var stored struct {
		ConfirmedTransfers []model.TransferOperation `json:"confirmed_transfers"`
	}
	if err := json.Unmarshal(output.New.InputSnapshotJSON, &stored); err != nil {
		t.Fatalf("decode transfers: %v", err)
	}
	if len(stored.ConfirmedTransfers) != 2 {
		t.Fatalf("recalculated run transfer count = %d, want 2", len(stored.ConfirmedTransfers))
	}
}

func TestBoundaryGateFlagsCoefficientVersionAndLeavesAcceptedUntouched(t *testing.T) {
	f := newGateFixture(t, "coefficient")
	tank := createGateTank(t, f, "GATE-C", "cv-2026.08")
	start := time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	createGateSnapshot(t, f, tank.ID, start.Add(-time.Hour), 10, constants.QualityGood)
	createGateSnapshot(t, f, tank.ID, end.Add(-time.Hour), 9.9, constants.QualityGood)
	openRun := runGateBalance(t, f, tank.ID, start, end)

	acceptedRun := runGateBalance(t, f, tank.ID, start, end)
	submitted, err := f.balances.Submit(context.Background(), acceptedRun.ID, dto.SubmitBalanceRequest{Version: acceptedRun.Version}, analystActor())
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	accepted, err := f.balances.Review(context.Background(), submitted.ID, dto.ReviewBalanceRequest{
		TargetStatus: constants.BalanceAccepted, Version: submitted.Version, ReviewNote: "证据完整接受，终态应不受系数更新影响",
	}, reviewerActor())
	if err != nil {
		t.Fatalf("accept: %v", err)
	}

	before, err := f.tanks.Get(context.Background(), tank.ID)
	if err != nil {
		t.Fatalf("get tank: %v", err)
	}
	request := dto.UpdateTankRequest{
		Name: before.Name, NominalCapacityM3: before.NominalCapacityM3, MinLevelM: before.MinLevelM, MaxLevelM: before.MaxLevelM,
		ReferenceDensityKGM3: before.ReferenceDensityKGM3, ReferenceTemperatureC: before.ReferenceTemperatureC,
		ThermalExpansionPerC: before.ThermalExpansionPerC, CapacityCurve: []float64{0, 10000},
		CoefficientVersion: "cv-2026.08", TankStatus: before.TankStatus, Version: before.Version,
	}
	// 仅改名称、系数版本不变：不置位。
	request.Name = "闸门验证储罐-改名"
	if _, err := f.tanks.Update(context.Background(), tank.ID, request, analystActor()); err != nil {
		t.Fatalf("rename tank: %v", err)
	}
	reloaded, _ := f.balances.Get(context.Background(), openRun.ID)
	if reloaded.BalanceStatus != constants.BalanceCalculating {
		t.Fatalf("status = %s, rename without coefficient change must not flag", reloaded.BalanceStatus)
	}

	before, _ = f.tanks.Get(context.Background(), tank.ID)
	request.Version = before.Version
	request.Name = before.Name
	request.CoefficientVersion = "cv-2026.09"
	if _, err := f.tanks.Update(context.Background(), tank.ID, request, analystActor()); err != nil {
		t.Fatalf("update coefficients: %v", err)
	}
	reloaded, _ = f.balances.Get(context.Background(), openRun.ID)
	if reloaded.BalanceStatus != constants.BalanceRecalculateRequired {
		t.Fatalf("status = %s, coefficient version change must flag open run", reloaded.BalanceStatus)
	}
	if reasons := gateReasons(t, reloaded); len(reasons) != 1 || reasons[0].Code != constants.RecalcReasonCoefficientVersion {
		t.Fatalf("unexpected coefficient reasons: %+v", reasons)
	}
	stillAccepted, _ := f.balances.Get(context.Background(), accepted.ID)
	if stillAccepted.BalanceStatus != constants.BalanceAccepted {
		t.Fatalf("accepted run status = %s, terminal accepted runs must remain untouched", stillAccepted.BalanceStatus)
	}
}

func TestRecalculateFailureKeepsOldRecordChainAndAuditIntact(t *testing.T) {
	f := newGateFixture(t, "rollback")
	tank := createGateTank(t, f, "GATE-R", "cv-v1")
	start := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	opening := createGateSnapshot(t, f, tank.ID, start.Add(-time.Hour), 10, constants.QualityGood)
	closing := createGateSnapshot(t, f, tank.ID, end.Add(-time.Hour), 9.9, constants.QualityGood)
	run := runGateBalance(t, f, tank.ID, start, end)
	_ = createGateSnapshot(t, f, tank.ID, start.Add(-15*time.Minute), 10.01, constants.QualityGood)
	flagged, err := f.balances.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("reload flagged: %v", err)
	}
	if flagged.BalanceStatus != constants.BalanceRecalculateRequired {
		t.Fatalf("status = %s, want recalculate_required", flagged.BalanceStatus)
	}

	// 直接删除期末快照，强制事务内重算在选取边界时失败。
	if err := f.db.Unscoped().Delete(&model.MeasurementSnapshot{}, closing.ID).Error; err != nil {
		t.Fatalf("delete closing snapshot: %v", err)
	}
	if _, err := f.balances.Recalculate(context.Background(), flagged.ID, dto.RecalculateBalanceRequest{Version: flagged.Version}, reviewerActor()); err == nil {
		t.Fatal("recalculation must fail when boundary evidence is unusable")
	}

	intact, err := f.balances.Get(context.Background(), flagged.ID)
	if err != nil {
		t.Fatalf("reload after failed recalculation: %v", err)
	}
	if intact.BalanceStatus != constants.BalanceRecalculateRequired || intact.SupersededByID != nil {
		t.Fatalf("old run must stay recalculate_required without chain link: %+v", intact)
	}
	if intact.Version != flagged.Version {
		t.Fatalf("old run version changed after failed replacement: %d -> %d", flagged.Version, intact.Version)
	}
	var runsCount int64
	if err := f.db.Model(&model.BalanceRun{}).Count(&runsCount).Error; err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runsCount != 1 {
		t.Fatalf("runs count = %d, failed replacement must not leave a new record", runsCount)
	}
	for _, action := range []string{"balance_run.recalculated", "balance_run.superseded"} {
		var count int64
		if err := f.db.Model(&model.AuditEvent{}).Where("action = ?", action).Count(&count).Error; err != nil {
			t.Fatalf("count audit %s: %v", action, err)
		}
		if count != 0 {
			t.Fatalf("audit %s count = %d, want 0 after rollback", action, count)
		}
	}
	if opening.ID == 0 {
		t.Fatal("opening fixture missing")
	}
}

func TestConcurrentRecalculateSucceedsExactlyOnce(t *testing.T) {
	f := newGateFixture(t, "concurrent")
	if sqlDB, err := f.db.DB(); err == nil {
		sqlDB.SetMaxOpenConns(4)
	}
	tank := createGateTank(t, f, "GATE-X", "cv-v1")
	start := time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	createGateSnapshot(t, f, tank.ID, start.Add(-time.Hour), 10, constants.QualityGood)
	createGateSnapshot(t, f, tank.ID, end.Add(-time.Hour), 9.9, constants.QualityGood)
	run := runGateBalance(t, f, tank.ID, start, end)
	createGateSnapshot(t, f, tank.ID, start.Add(-20*time.Minute), 10.02, constants.QualityGood)
	flagged, err := f.balances.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("reload flagged: %v", err)
	}
	if flagged.BalanceStatus != constants.BalanceRecalculateRequired {
		t.Fatalf("status = %s, want recalculate_required", flagged.BalanceStatus)
	}

	const contenders = 8
	type outcome struct {
		ok  bool
		err error
	}
	results := make(chan outcome, contenders)
	barrier := make(chan struct{})
	for i := 0; i < contenders; i++ {
		go func() {
			<-barrier
			_, recalcErr := f.balances.Recalculate(context.Background(), flagged.ID, dto.RecalculateBalanceRequest{Version: flagged.Version}, reviewerActor())
			results <- outcome{ok: recalcErr == nil, err: recalcErr}
		}()
	}
	close(barrier)

	successes := 0
	var firstError error
	for i := 0; i < contenders; i++ {
		outcome := <-results
		if outcome.ok {
			successes++
		} else if firstError == nil {
			firstError = outcome.err
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent replacement successes = %d, want exactly 1; first error: %v", successes, firstError)
	}
	var successors int64
	if err := f.db.Model(&model.BalanceRun{}).Where("supersedes_id = ?", flagged.ID).Count(&successors).Error; err != nil {
		t.Fatalf("count successors: %v", err)
	}
	if successors != 1 {
		t.Fatalf("successor chain count = %d, want 1", successors)
	}
	var pending int64
	if err := f.db.Model(&model.BalanceRun{}).
		Where("supersedes_id = ? AND balance_status = ?", flagged.ID, constants.BalancePendingReview).
		Count(&pending).Error; err != nil {
		t.Fatalf("count pending successor: %v", err)
	}
	if pending != 1 {
		t.Fatalf("pending successor count = %d, want 1", pending)
	}
}

func TestMergeRecalculationReasonsDeduplicates(t *testing.T) {
	at := "2026-06-01T00:00:00Z"
	first := []dto.RecalculationReason{
		{Code: constants.RecalcReasonBoundarySnapshotCloser, EntityType: "measurement_snapshot", EntityID: 7, Detail: "first", DetectedAt: at},
	}
	additions := []dto.RecalculationReason{
		{Code: constants.RecalcReasonBoundarySnapshotCloser, EntityType: "measurement_snapshot", EntityID: 7, Detail: "duplicate", DetectedAt: at},
		{Code: constants.RecalcReasonLateTransferConfirmed, EntityType: "transfer_operation", EntityID: 9, Detail: "new", DetectedAt: at},
	}
	merged := dto.MergeRecalculationReasons(first, additions...)
	if len(merged) != 2 {
		t.Fatalf("merged length = %d, want 2", len(merged))
	}
	if merged[0].Detail != "first" {
		t.Fatalf("first detection detail must be preserved, got %q", merged[0].Detail)
	}
	// 确保 JSON 字段可直接作为 datatypes.JSON 使用。
	raw, err := json.Marshal(merged)
	if err != nil {
		t.Fatalf("marshal merged reasons: %v", err)
	}
	_ = datatypes.JSON(raw)
}
