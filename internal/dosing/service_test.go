package dosing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/journal"
	"github.com/wyw14/cry-97/internal/model"
)

type fakeBus struct {
	commands []model.DeviceCommand
	fail     error
}

func (b *fakeBus) Publish(_ context.Context, command model.DeviceCommand) error {
	if b.fail != nil {
		return b.fail
	}
	b.commands = append(b.commands, command)
	return nil
}

type captureScheduler struct{ items []journal.Compensation }

func (c *captureScheduler) Schedule(item journal.Compensation) { c.items = append(c.items, item) }

func newTestService(t *testing.T, policy RiskPolicy) (*Service, *fakeBus, *captureScheduler, *journal.FileStore, model.LineID) {
	t.Helper()
	store, err := journal.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bus := &fakeBus{}
	service, err := NewService(store, bus, NewLedger())
	if err != nil {
		t.Fatal(err)
	}
	scheduler := &captureScheduler{}
	service.WithCompensations(scheduler).WithPolicy(policy)
	return service, bus, scheduler, store, model.LineID("line-1")
}

func permissivePolicy() RiskPolicy {
	// 相对阈值 50%，首次限值 50，无绝对上限。
	return RiskPolicy{RelativeThreshold: 0.5, AbsoluteStep: 50, RateCeiling: 0}
}

func TestNormalChangeGoesStraightToBus(t *testing.T) {
	service, bus, scheduler, _, lineID := newTestService(t, permissivePolicy())
	batchID := uuid.New()
	now := time.Unix(1700000000, 0).UTC()

	outcome, err := service.Submit(context.Background(), SubmitInput{
		LineID: lineID, BatchID: batchID, Chemical: "pac", Rate: 10,
		Generation: 1, RequestedBy: "operator-a",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Risk != RiskNormal || outcome.Command == nil {
		t.Fatalf("normal change must carry a direct command, got %+v", outcome)
	}
	if len(bus.commands) != 1 || bus.commands[0].Action != "set" || bus.commands[0].Setpoint != 10 {
		t.Fatalf("unexpected bus commands: %+v", bus.commands)
	}
	if len(scheduler.items) != 0 {
		t.Fatal("normal change must not schedule compensations")
	}
	if len(service.AllRequests()) != 0 {
		t.Fatal("normal change must not create an approval request")
	}
}

func TestHighRiskRequestNeverReachesBusBeforeApproval(t *testing.T) {
	service, bus, _, _, lineID := newTestService(t, permissivePolicy())
	batchID := uuid.New()
	ctx := context.Background()
	now := time.Unix(1700000000, 0).UTC()

	// 先有一个在运旧值 10。
	if _, err := service.Submit(ctx, SubmitInput{
		LineID: lineID, BatchID: batchID, Chemical: "pac", Rate: 10,
		Generation: 1, RequestedBy: "operator-a",
	}, now); err != nil {
		t.Fatal(err)
	}
	bus.commands = nil

	// 10 -> 30 为 200% 变化，属于高风险。
	outcome, err := service.Submit(ctx, SubmitInput{
		LineID: lineID, BatchID: batchID, Chemical: "pac", Rate: 30,
		Generation: 1, RequestedBy: "operator-a",
	}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Risk != RiskHigh || outcome.Request == nil || outcome.Request.Status != StatusPending {
		t.Fatalf("expected pending high-risk request, got %+v", outcome)
	}
	if outcome.Request.PreviousRate != 10 || outcome.Request.Rate != 30 ||
		outcome.Request.Chemical != "pac" || outcome.Request.BatchID != batchID {
		t.Fatalf("request must retain old/new/chemical/batch: %+v", outcome.Request)
	}
	if len(bus.commands) != 0 {
		t.Fatalf("unapproved high-risk change must not enter the device bus: %+v", bus.commands)
	}
	if current, ok := service.ledger.Current(batchID, "pac"); !ok || current.Rate != 10 {
		t.Fatalf("ledger current rate must stay at old value 10, got %+v", current)
	}

	// 申请人不能审核自己的变更。
	if _, _, _, err := service.Approve(ctx, outcome.Request.ID, "operator-a", "", now.Add(2*time.Minute)); err == nil {
		t.Fatal("self-approval must be rejected")
	}
	if len(bus.commands) != 0 {
		t.Fatal("failed approval must not publish anything")
	}

	// 他人批准后才进入总线。
	_, change, command, err := service.Approve(ctx, outcome.Request.ID, "supervisor-b", "checked", now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if command.Setpoint != 30 || command.Metadata["request_id"] != outcome.Request.ID.String() {
		t.Fatalf("approved command missing request linkage: %+v", command)
	}
	if change.PreviousRate != 10 || change.Rate != 30 {
		t.Fatalf("approved change must record old and new rate: %+v", change)
	}
	if current, _ := service.ledger.Current(batchID, "pac"); current.Rate != 30 {
		t.Fatalf("ledger must advance to 30 after approval, got %v", current.Rate)
	}

	// 已决申请不能再次批准。
	if _, _, _, err := service.Approve(ctx, outcome.Request.ID, "supervisor-c", "", now.Add(3*time.Minute)); err == nil {
		t.Fatal("already decided request cannot be approved again")
	}
}

func TestRejectNeverPublishes(t *testing.T) {
	service, bus, _, _, lineID := newTestService(t, permissivePolicy())
	batchID := uuid.New()
	ctx := context.Background()
	now := time.Unix(1700000000, 0).UTC()

	outcome, err := service.Submit(ctx, SubmitInput{
		LineID: lineID, BatchID: batchID, Chemical: "pam", Rate: 60,
		Generation: 1, RequestedBy: "operator-a",
	}, now) // 首次 60 > 50，高风险
	if err != nil {
		t.Fatal(err)
	}
	request, err := service.Reject(ctx, outcome.Request.ID, "supervisor-b", "too high", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if request.Status != StatusRejected {
		t.Fatalf("status = %s, want rejected", request.Status)
	}
	if len(bus.commands) != 0 {
		t.Fatalf("rejected change must never enter the bus: %+v", bus.commands)
	}
	if _, ok := service.ledger.Current(batchID, "pam"); ok {
		t.Fatal("rejected change must not be recorded in the effective ledger")
	}
}

func TestRevokePendingDoesNotCompensate(t *testing.T) {
	service, bus, scheduler, _, lineID := newTestService(t, permissivePolicy())
	batchID := uuid.New()
	ctx := context.Background()
	now := time.Unix(1700000000, 0).UTC()

	outcome, _ := service.Submit(ctx, SubmitInput{
		LineID: lineID, BatchID: batchID, Chemical: "pac", Rate: 60,
		Generation: 1, RequestedBy: "operator-a",
	}, now)
	request, revert, err := service.Revoke(ctx, outcome.Request.ID, "operator-a", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if request.Status != StatusRevoked || revert != nil {
		t.Fatalf("pending revoke must close request without bus command: %+v", request)
	}
	if len(scheduler.items) != 0 || len(bus.commands) != 0 {
		t.Fatal("pending revoke must not generate compensation or bus traffic")
	}
}

func TestRevokeIssuedRevertsToOldValueAndCompensates(t *testing.T) {
	service, bus, scheduler, _, lineID := newTestService(t, permissivePolicy())
	batchID := uuid.New()
	ctx := context.Background()
	now := time.Unix(1700000000, 0).UTC()

	// 旧值 10。
	if _, err := service.Submit(ctx, SubmitInput{
		LineID: lineID, BatchID: batchID, Chemical: "pac", Rate: 10,
		Generation: 1, RequestedBy: "operator-a",
	}, now); err != nil {
		t.Fatal(err)
	}
	// 高风险 10 -> 30 并批准。
	outcome, _ := service.Submit(ctx, SubmitInput{
		LineID: lineID, BatchID: batchID, Chemical: "pac", Rate: 30,
		Generation: 1, RequestedBy: "operator-a",
	}, now.Add(time.Minute))
	if _, _, _, err := service.Approve(ctx, outcome.Request.ID, "supervisor-b", "", now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}

	request, revert, err := service.Revoke(ctx, outcome.Request.ID, "supervisor-b", now.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if request.Status != StatusRevoked {
		t.Fatalf("status = %s, want revoked", request.Status)
	}
	if revert == nil || revert.Action != "revert" || revert.Setpoint != 10 {
		t.Fatalf("revoke must issue revert to old value 10, got %+v", revert)
	}
	if len(scheduler.items) != 1 {
		t.Fatalf("expected one safety compensation, got %d", len(scheduler.items))
	}
	compensation := scheduler.items[0]
	if compensation.Action != "revert" || compensation.SequenceID != outcome.Request.ID {
		t.Fatalf("compensation must target the revoked request: %+v", compensation)
	}
	history := service.History(batchID)
	if len(history) != 3 { // 直发 10、批准 30、撤销回退 10
		t.Fatalf("history must preserve all effective changes, got %d", len(history))
	}
	last := history[len(history)-1]
	if last.Rate != 10 || last.PreviousRate != 30 || last.RequestID != outcome.Request.ID {
		t.Fatalf("revert change must restore old value with request link: %+v", last)
	}

	// 维护任务执行补偿：再次下发回退旧值命令。
	bus.commands = nil
	if err := service.ExecuteCompensation(ctx, compensation); err != nil {
		t.Fatal(err)
	}
	if len(bus.commands) != 1 || bus.commands[0].Action != "revert" || bus.commands[0].Setpoint != 10 {
		t.Fatalf("compensation must republish revert to old value: %+v", bus.commands)
	}
}

func TestReplayRestoresLedgerAndRequestsWithoutReissuingPending(t *testing.T) {
	service, _, _, reader, lineID := newTestService(t, permissivePolicy())
	batchID := uuid.New()
	ctx := context.Background()
	now := time.Unix(1700000000, 0).UTC()

	if _, err := service.Submit(ctx, SubmitInput{
		LineID: lineID, BatchID: batchID, Chemical: "pac", Rate: 10,
		Generation: 1, RequestedBy: "operator-a",
	}, now); err != nil {
		t.Fatal(err)
	}
	pending, err := service.Submit(ctx, SubmitInput{
		LineID: lineID, BatchID: batchID, Chemical: "pac", Rate: 30,
		Generation: 1, RequestedBy: "operator-a",
	}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	// 全新服务实例（新内存总线与台账）回放同一日志。
	replayBus := &fakeBus{}
	replayed, err := NewService(reader, replayBus, NewLedger())
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events(ctx, lineID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if err := replayed.Restore(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	// 只有已生效的 10 进入总线，待审 30 绝不重发。
	if len(replayBus.commands) != 1 || replayBus.commands[0].Setpoint != 10 {
		t.Fatalf("replay must only reissue effective commands: %+v", replayBus.commands)
	}
	if current, ok := replayed.ledger.Current(batchID, "pac"); !ok || current.Rate != 10 {
		t.Fatalf("replayed ledger current rate must be 10, got %+v", current)
	}
	restored, ok := replayed.GetRequest(pending.Request.ID)
	if !ok || restored.Status != StatusPending {
		t.Fatalf("pending request must be restored for review, got %+v", restored)
	}
}

func TestStaleReplayCommandIsSkipped(t *testing.T) {
	service, _, _, reader, lineID := newTestService(t, permissivePolicy())
	batchID := uuid.New()
	ctx := context.Background()
	now := time.Unix(1700000000, 0).UTC()

	if _, err := service.Submit(ctx, SubmitInput{
		LineID: lineID, BatchID: batchID, Chemical: "pac", Rate: 10,
		Generation: 1, RequestedBy: "operator-a",
	}, now); err != nil {
		t.Fatal(err)
	}
	replayBus := &fakeBus{fail: model.ErrStaleCommand}
	replayed, err := NewService(reader, replayBus, NewLedger())
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events(ctx, lineID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if err := replayed.Restore(ctx, event); err != nil {
			t.Fatalf("stale commands must be skipped without error: %v", err)
		}
	}
	if _, ok := replayed.ledger.Current(batchID, "pac"); ok {
		t.Fatal("skipped stale command must not become the current ledger value")
	}
	if !errors.Is(model.ErrStaleCommand, model.ErrStaleCommand) {
		t.Fatal("sentinel identity broken")
	}
}
