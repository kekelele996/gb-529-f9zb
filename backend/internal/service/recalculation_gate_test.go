package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"lng-boiloff-gas-balance/backend/internal/constants"
	"lng-boiloff-gas-balance/backend/internal/dto"
	"lng-boiloff-gas-balance/backend/internal/model"
	"lng-boiloff-gas-balance/backend/internal/repository"
	"lng-boiloff-gas-balance/backend/pkg/api"
)

func apiCode(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var appErr *api.Error
	if !asError(err, &appErr) {
		t.Fatalf("expected api.Error, got %T: %v", err, err)
	}
	return appErr.Code
}

func asError(err error, target **api.Error) bool {
	for err != nil {
		if e, ok := err.(*api.Error); ok {
			*target = e
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

type gateFixture struct {
	db           *gorm.DB
	tank         model.StorageTank
	analyst      repository.Actor
	reviewer     repository.Actor
	balances     *BalanceService
	tanks        *TankService
	measurements *MeasurementService
	transfers    *TransferService
}

func setupGateFixture(t *testing.T, suffix string) gateFixture {
	t.Helper()
	dsn := "file:gate-" + suffix + "-" + time.Now().Format("150405.000000") + "?mode=memory&cache=shared"
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
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	tankRepo := repository.NewTankRepository(db)
	measurementRepo := repository.NewMeasurementRepository(db)
	transferRepo := repository.NewTransferRepository(db)
	balanceRepo := repository.NewBalanceRepository(db)
	fixture := gateFixture{
		db:           db,
		analyst:      repository.Actor{UserID: 1, Email: "analyst@lng.local", Role: constants.RoleProcessAnalyst, RequestID: "test-analyst"},
		reviewer:     repository.Actor{UserID: 2, Email: "reviewer@lng.local", Role: constants.RoleReviewer, RequestID: "test-reviewer"},
		balances:     NewBalanceService(balanceRepo, tankRepo, measurementRepo, transferRepo),
		tanks:        NewTankService(tankRepo),
		measurements: NewMeasurementService(measurementRepo, tankRepo),
		transfers:    NewTransferService(transferRepo, tankRepo),
	}
	created, err := fixture.tanks.Create(context.Background(), dto.CreateTankRequest{
		TankCode:              "TK-GATE-" + suffix,
		Name:                  "gate tank",
		NominalCapacityM3:     200000,
		MinLevelM:             0,
		MaxLevelM:             20,
		ReferenceDensityKGM3:  450,
		ReferenceTemperatureC: -160,
		ThermalExpansionPerC:  0.0012,
		CapacityCurve:         []float64{0, 10000},
		CoefficientVersion:    "cv-v1",
		TankStatus:            "active",
	}, fixture.analyst)
	if err != nil {
		t.Fatalf("create tank: %v", err)
	}
	fixture.tank = created
	return fixture
}

func (f gateFixture) mustSnapshot(t *testing.T, measuredAt string, level float64, quality constants.QualityFlag, note string) model.MeasurementSnapshot {
	t.Helper()
	at, _ := time.Parse(time.RFC3339, measuredAt)
	item, err := f.measurements.Create(context.Background(), dto.CreateMeasurementRequest{
		TankID:                    f.tank.ID,
		MeasuredAt:                &at,
		LiquidLevelM:              level,
		LiquidTempC:               -160,
		VaporPressureKPA:          115,
		DensityKGM3:               450,
		MeasurementUncertaintyPct: 0.35,
		QualityFlag:               string(quality),
		SourceNote:                note,
	}, f.analyst)
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	return item
}

func (f gateFixture) mustTransfer(t *testing.T, operation string, start, end string, mass float64, status string) model.TransferOperation {
	t.Helper()
	startAt, _ := time.Parse(time.RFC3339, start)
	endAt, _ := time.Parse(time.RFC3339, end)
	item, err := f.transfers.Create(context.Background(), dto.CreateTransferRequest{
		TankID:                    f.tank.ID,
		OperationType:             operation,
		StartAt:                   &startAt,
		EndAt:                     &endAt,
		MeasuredMassKG:            mass,
		MeasurementUncertaintyPct: 0.25,
		CounterpartyRef:           "METER-" + operation + "-" + strings.ReplaceAll(start, ":", ""),
		OperationStatus:           status,
	}, f.analyst)
	if err != nil {
		t.Fatalf("create transfer: %v", err)
	}
	return item
}

func runGatePeriod() (*time.Time, *time.Time) {
	start, _ := time.Parse(time.RFC3339, "2026-07-01T01:00:00Z")
	end, _ := time.Parse(time.RFC3339, "2026-07-02T01:00:00Z")
	return &start, &end
}

func TestBoundarySnapshotGateBlocksReviewAndDrivesReplacement(t *testing.T) {
	ctx := context.Background()
	fixture := setupGateFixture(t, "SNAP")
	fixture.mustSnapshot(t, "2026-07-01T00:30:00Z", 10, constants.QualityGood, "opening")
	fixture.mustSnapshot(t, "2026-07-02T00:30:00Z", 9.96, constants.QualityGood, "closing")
	fixture.mustTransfer(t, "inflow", "2026-07-01T03:00:00Z", "2026-07-01T04:00:00Z", 100000, "confirmed")
	fixture.mustTransfer(t, "outflow", "2026-07-01T05:00:00Z", "2026-07-01T06:00:00Z", 65000, "confirmed")
	start, end := runGatePeriod()
	original, err := fixture.balances.Run(ctx, dto.RunBalanceRequest{TankID: fixture.tank.ID, PeriodStart: start, PeriodEnd: end}, fixture.analyst)
	if err != nil {
		t.Fatalf("run balance: %v", err)
	}

	// 补录更接近期末边界的有效快照，原运行立即转为待重算。
	fixture.mustSnapshot(t, "2026-07-02T00:45:00Z", 9.95, constants.QualityGood, "later closing")
	flagged, err := fixture.balances.Get(ctx, original.ID)
	if err != nil {
		t.Fatalf("reload flagged run: %v", err)
	}
	if flagged.BalanceStatus != constants.BalanceRecalculationRequired {
		t.Fatalf("status = %s, want recalculation_required", flagged.BalanceStatus)
	}
	reasons := flagged.ParseRecalculationReasons()
	if len(reasons) != 1 || reasons[0].Code != repository.GateReasonBoundarySnapshot {
		t.Fatalf("unexpected gate reasons: %+v", reasons)
	}
	if flagged.SupersedesID != nil || flagged.SupersededByID != nil {
		t.Fatal("pending recalculation run must not carry replacement links yet")
	}

	// 复核员不能直接接受或驳回待重算运行。
	_, err = fixture.balances.Review(ctx, flagged.ID, dto.ReviewBalanceRequest{
		TargetStatus: constants.BalanceAccepted, Version: flagged.Version, ReviewNote: "should be blocked",
	}, fixture.reviewer)
	if code := apiCode(t, err); code != "RECALCULATION_REQUIRED" {
		t.Fatalf("review gate code = %s, want RECALCULATION_REQUIRED", code)
	}

	// 重新计算生成后继，并原子申领旧记录。
	successor, err := fixture.balances.Recalculate(ctx, flagged.ID, dto.RecalculateBalanceRequest{Version: flagged.Version}, fixture.analyst)
	if err != nil {
		t.Fatalf("recalculate: %v", err)
	}
	if successor.SupersedesID == nil || *successor.SupersedesID != flagged.ID || successor.ID == flagged.ID {
		t.Fatalf("successor chain broken: %+v", successor)
	}
	if successor.ClosingMassKG == flagged.ClosingMassKG {
		t.Fatal("recalculation must consume the later boundary snapshot")
	}
	claimed, err := fixture.balances.Get(ctx, flagged.ID)
	if err != nil {
		t.Fatalf("reload claimed run: %v", err)
	}
	if claimed.SupersededByID == nil || *claimed.SupersededByID != successor.ID {
		t.Fatalf("predecessor not linked to successor: %+v", claimed)
	}

	// 重复重算只能成功一次。
	_, err = fixture.balances.Recalculate(ctx, flagged.ID, dto.RecalculateBalanceRequest{Version: claimed.Version}, fixture.analyst)
	if code := apiCode(t, err); code != "RECALCULATION_ALREADY_EXISTS" {
		t.Fatalf("duplicate recalculation code = %s, want RECALCULATION_ALREADY_EXISTS", code)
	}

	// 后继提交复核；直接 review 接受后继也被拒绝，必须走原子替代。
	submitted, err := fixture.balances.Submit(ctx, successor.ID, dto.SubmitBalanceRequest{Version: successor.Version}, fixture.analyst)
	if err != nil {
		t.Fatalf("submit successor: %v", err)
	}
	_, err = fixture.balances.Review(ctx, successor.ID, dto.ReviewBalanceRequest{
		TargetStatus: constants.BalanceAccepted, Version: submitted.Version, ReviewNote: "must replace atomically",
	}, fixture.reviewer)
	if code := apiCode(t, err); code != "REPLACEMENT_REQUIRED" {
		t.Fatalf("direct successor review code = %s, want REPLACEMENT_REQUIRED", code)
	}

	// 复核员原子替代：后继 accepted，旧记录 replaced。
	accepted, err := fixture.balances.Replace(ctx, flagged.ID, dto.ReplaceBalanceRequest{
		SuccessorID: successor.ID, PredecessorVersion: claimed.Version, SuccessorVersion: submitted.Version,
		ReviewNote: "后继证据复核通过，原子替代旧记录。",
	}, fixture.reviewer)
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if accepted.BalanceStatus != constants.BalanceAccepted || accepted.ReviewedBy == nil {
		t.Fatalf("successor not accepted: %+v", accepted)
	}
	replaced, err := fixture.balances.Get(ctx, flagged.ID)
	if err != nil {
		t.Fatalf("reload replaced run: %v", err)
	}
	if replaced.BalanceStatus != constants.BalanceReplaced {
		t.Fatalf("predecessor status = %s, want replaced", replaced.BalanceStatus)
	}

	// 重复替代必须失败且状态保持终态。
	_, err = fixture.balances.Replace(ctx, flagged.ID, dto.ReplaceBalanceRequest{
		SuccessorID: successor.ID, PredecessorVersion: replaced.Version, SuccessorVersion: accepted.Version,
		ReviewNote: "duplicate replacement must fail",
	}, fixture.reviewer)
	if code := apiCode(t, err); code != "REPLACEMENT_NOT_PENDING" {
		t.Fatalf("duplicate replacement code = %s, want REPLACEMENT_NOT_PENDING", code)
	}
	finalOld, _ := fixture.balances.Get(ctx, flagged.ID)
	finalNew, _ := fixture.balances.Get(ctx, successor.ID)
	if finalOld.BalanceStatus != constants.BalanceReplaced || finalNew.BalanceStatus != constants.BalanceAccepted {
		t.Fatalf("final states drifted after failed replacement: %s/%s", finalOld.BalanceStatus, finalNew.BalanceStatus)
	}
}

func TestLateTransferConfirmationGate(t *testing.T) {
	ctx := context.Background()
	fixture := setupGateFixture(t, "XFER")
	fixture.mustSnapshot(t, "2026-07-01T00:30:00Z", 10, constants.QualityGood, "opening")
	fixture.mustSnapshot(t, "2026-07-02T00:30:00Z", 9.96, constants.QualityGood, "closing")
	fixture.mustTransfer(t, "inflow", "2026-07-01T03:00:00Z", "2026-07-01T04:00:00Z", 100000, "confirmed")
	start, end := runGatePeriod()
	original, err := fixture.balances.Run(ctx, dto.RunBalanceRequest{TankID: fixture.tank.ID, PeriodStart: start, PeriodEnd: end}, fixture.analyst)
	if err != nil {
		t.Fatalf("run balance: %v", err)
	}

	// 运行存证后登记草稿转移，不应命中闸门。
	draft := fixture.mustTransfer(t, "outflow", "2026-07-01T08:00:00Z", "2026-07-01T09:00:00Z", 42000, "draft")
	stillGood, err := fixture.balances.Get(ctx, original.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if stillGood.BalanceStatus != constants.BalanceCalculating {
		t.Fatalf("draft transfer must not gate, got %s", stillGood.BalanceStatus)
	}

	// 晚确认落入期间的流出，立即挂起重算。
	if _, err := fixture.transfers.Transition(ctx, draft.ID, dto.TransitionTransferRequest{
		TargetStatus: "confirmed", Version: draft.Version, Reason: "班次后补确认流出计量",
	}, fixture.analyst); err != nil {
		t.Fatalf("confirm late transfer: %v", err)
	}
	flagged, err := fixture.balances.Get(ctx, original.ID)
	if err != nil {
		t.Fatalf("reload flagged: %v", err)
	}
	if flagged.BalanceStatus != constants.BalanceRecalculationRequired {
		t.Fatalf("status = %s, want recalculation_required", flagged.BalanceStatus)
	}
	reasons := flagged.ParseRecalculationReasons()
	if len(reasons) != 1 || reasons[0].Code != repository.GateReasonLateTransfer ||
		reasons[0].EvidenceRef != "transfer_operation:"+itoa(draft.ID) {
		t.Fatalf("unexpected gate reasons: %+v", reasons)
	}
}

func TestCoefficientVersionGate(t *testing.T) {
	ctx := context.Background()
	fixture := setupGateFixture(t, "COEF")
	fixture.mustSnapshot(t, "2026-07-01T00:30:00Z", 10, constants.QualityGood, "opening")
	fixture.mustSnapshot(t, "2026-07-02T00:30:00Z", 9.96, constants.QualityGood, "closing")
	start, end := runGatePeriod()
	original, err := fixture.balances.Run(ctx, dto.RunBalanceRequest{TankID: fixture.tank.ID, PeriodStart: start, PeriodEnd: end}, fixture.analyst)
	if err != nil {
		t.Fatalf("run balance: %v", err)
	}
	if _, err := fixture.balances.Submit(ctx, original.ID, dto.SubmitBalanceRequest{Version: original.Version}, fixture.analyst); err != nil {
		t.Fatalf("submit: %v", err)
	}
	update := dto.UpdateTankRequest{
		Name: fixture.tank.Name, NominalCapacityM3: fixture.tank.NominalCapacityM3,
		MinLevelM: fixture.tank.MinLevelM, MaxLevelM: fixture.tank.MaxLevelM,
		ReferenceDensityKGM3: fixture.tank.ReferenceDensityKGM3, ReferenceTemperatureC: fixture.tank.ReferenceTemperatureC,
		ThermalExpansionPerC: fixture.tank.ThermalExpansionPerC, CapacityCurve: []float64{0, 10000},
		CoefficientVersion: "cv-v2", TankStatus: "active", Version: fixture.tank.Version,
	}
	if _, err := fixture.tanks.Update(ctx, fixture.tank.ID, update, fixture.analyst); err != nil {
		t.Fatalf("update coefficients: %v", err)
	}
	flagged, err := fixture.balances.Get(ctx, original.ID)
	if err != nil {
		t.Fatalf("reload flagged: %v", err)
	}
	if flagged.BalanceStatus != constants.BalanceRecalculationRequired {
		t.Fatalf("status = %s, want recalculation_required", flagged.BalanceStatus)
	}
	reasons := flagged.ParseRecalculationReasons()
	if len(reasons) != 1 || reasons[0].Code != repository.GateReasonCoefficient {
		t.Fatalf("unexpected gate reasons: %+v", reasons)
	}

	// 相同版本字符串的再次保存不应重复命中。
	update.Version = 2
	if _, err := fixture.tanks.Update(ctx, fixture.tank.ID, update, fixture.analyst); err != nil {
		t.Fatalf("second coefficient save: %v", err)
	}
	again, _ := fixture.balances.Get(ctx, original.ID)
	if len(again.ParseRecalculationReasons()) != 1 {
		t.Fatalf("identical coefficient version must not append reasons: %+v", again.ParseRecalculationReasons())
	}
}

func TestFinalizedRunsAreImmuneToGate(t *testing.T) {
	ctx := context.Background()
	fixture := setupGateFixture(t, "FINAL")
	fixture.mustSnapshot(t, "2026-07-01T00:30:00Z", 10, constants.QualityGood, "opening")
	fixture.mustSnapshot(t, "2026-07-02T00:30:00Z", 9.96, constants.QualityGood, "closing")
	fixture.mustTransfer(t, "inflow", "2026-07-01T03:00:00Z", "2026-07-01T04:00:00Z", 100000, "confirmed")
	fixture.mustTransfer(t, "outflow", "2026-07-01T05:00:00Z", "2026-07-01T06:00:00Z", 65000, "confirmed")
	start, end := runGatePeriod()
	original, err := fixture.balances.Run(ctx, dto.RunBalanceRequest{TankID: fixture.tank.ID, PeriodStart: start, PeriodEnd: end}, fixture.analyst)
	if err != nil {
		t.Fatalf("run balance: %v", err)
	}
	submitted, err := fixture.balances.Submit(ctx, original.ID, dto.SubmitBalanceRequest{Version: original.Version}, fixture.analyst)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := fixture.balances.Review(ctx, submitted.ID, dto.ReviewBalanceRequest{
		TargetStatus: constants.BalanceAccepted, Version: submitted.Version, ReviewNote: "接受终态结果",
	}, fixture.reviewer); err != nil {
		t.Fatalf("review: %v", err)
	}
	// 已接受运行的证据事后再变化，终态保持不可变。
	fixture.mustSnapshot(t, "2026-07-02T00:45:00Z", 9.94, constants.QualityGood, "late evidence")
	finalRun, err := fixture.balances.Get(ctx, original.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if finalRun.BalanceStatus != constants.BalanceAccepted {
		t.Fatalf("accepted run changed to %s", finalRun.BalanceStatus)
	}
	if len(finalRun.ParseRecalculationReasons()) != 0 {
		t.Fatal("finalized run must never carry gate reasons")
	}
}

func itoa(value uint) string {
	if value == 0 {
		return "0"
	}
	digits := make([]byte, 0, 12)
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}
