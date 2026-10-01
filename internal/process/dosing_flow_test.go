package process

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/dosing"
	"github.com/wyw14/cry-97/internal/model"
)

func newTestPlant(t *testing.T) (*Plant, model.LineID) {
	t.Helper()
	clock := time.Unix(1700000000, 0).UTC
	plant, err := NewPlant(t.TempDir(), []model.LineID{"line-1"}, clock)
	if err != nil {
		t.Fatal(err)
	}
	return plant, "line-1"
}

func activeBatchID(t *testing.T, plant *Plant, lineID model.LineID) uuid.UUID {
	t.Helper()
	batch, err := plant.StartBatch(context.Background(), lineID, 760, "north-intake")
	if err != nil {
		t.Fatal(err)
	}
	return batch.ID
}

func doserSetpoint(plant *Plant, chemical string) (float64, bool) {
	for _, command := range plant.devices.All() {
		if command.DeviceID == "doser-"+chemical {
			return command.Setpoint, true
		}
	}
	return 0, false
}

func TestPlantHighRiskDoseRequiresApprovalAndRevokeCompensates(t *testing.T) {
	plant, lineID := newTestPlant(t)
	ctx := context.Background()
	batchID := activeBatchID(t, plant, lineID)

	// 首次小剂量 10：普通范围，直接下发。
	direct, err := plant.ChangeDose(ctx, lineID, batchID, "pac", 10, "operator-a")
	if err != nil {
		t.Fatal(err)
	}
	if direct.Risk != dosing.RiskNormal {
		t.Fatalf("small first dose must be normal, got %s", direct.Risk)
	}
	if setpoint, ok := doserSetpoint(plant, "pac"); !ok || setpoint != 10 {
		t.Fatalf("bus setpoint must be 10, got %v/%v", setpoint, ok)
	}

	// 10 -> 100：高风险（相对变化远超 25% 且接近上限）。
	outcome, err := plant.ChangeDose(ctx, lineID, batchID, "pac", 100, "operator-a")
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Risk != dosing.RiskHigh || outcome.Request == nil {
		t.Fatalf("expected high-risk pending request, got %+v", outcome)
	}
	requestID := outcome.Request.ID

	// 批准前总线必须仍是旧值 10。
	if setpoint, _ := doserSetpoint(plant, "pac"); setpoint != 10 {
		t.Fatalf("unapproved change must not reach bus, setpoint=%v", setpoint)
	}

	// 申请人自审被拒。
	if _, _, _, err := plant.ApproveDoseChange(ctx, requestID, "operator-a", ""); err == nil {
		t.Fatal("self approval must be rejected")
	}

	// 他人批准后下发新值。
	if _, _, _, err := plant.ApproveDoseChange(ctx, requestID, "supervisor-b", "ok"); err != nil {
		t.Fatal(err)
	}
	if setpoint, _ := doserSetpoint(plant, "pac"); setpoint != 100 {
		t.Fatalf("approved change must set bus to 100, got %v", setpoint)
	}

	// 撤销已下发变更：立即回退旧值并登记安全补偿。
	revoked, revert, err := plant.RevokeDoseChange(ctx, requestID, "supervisor-b")
	if err != nil {
		t.Fatal(err)
	}
	if revoked.Status != dosing.StatusRevoked || revert == nil || revert.Setpoint != 10 {
		t.Fatalf("revoke must revert bus to old value 10, request=%+v command=%+v", revoked, revert)
	}
	pending := plant.compensations.Pending()
	if len(pending) != 1 || pending[0].Action != "revert" {
		t.Fatalf("expected one revert compensation, got %+v", pending)
	}

	// 历史完整保留：10 → 100 → 回退 10。
	history := plant.DoseHistory(batchID)
	if len(history) != 3 {
		t.Fatalf("dose history must retain all effective changes, got %d", len(history))
	}

	// 推进时钟超过补偿到期时间，维护任务重发回退命令且不出错。
	plant.now = func() time.Time { return time.Unix(1700000010, 0).UTC() }
	count, err := plant.RunCompensations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected 1 executed compensation, got %d", count)
	}
	if len(plant.compensations.Pending()) != 0 {
		t.Fatal("executed compensation must leave the queue")
	}
}

func TestPlantRejectedHighRiskDoseNeverReachesBus(t *testing.T) {
	plant, lineID := newTestPlant(t)
	ctx := context.Background()
	batchID := activeBatchID(t, plant, lineID)

	if _, err := plant.ChangeDose(ctx, lineID, batchID, "pam", 10, "operator-a"); err != nil {
		t.Fatal(err)
	}
	outcome, err := plant.ChangeDose(ctx, lineID, batchID, "pam", 100, "operator-a")
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Risk != dosing.RiskHigh {
		t.Fatalf("10->100 must be high risk, got %s", outcome.Risk)
	}
	if _, err := plant.RejectDoseChange(ctx, outcome.Request.ID, "supervisor-b", "no"); err != nil {
		t.Fatal(err)
	}
	// pam 的总线上只有最初的 10，100 从未出现。
	if setpoint, ok := doserSetpoint(plant, "pam"); !ok || setpoint != 10 {
		t.Fatalf("rejected change must not reach bus, setpoint=%v ok=%v", setpoint, ok)
	}
}
