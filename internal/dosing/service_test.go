package dosing

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/model"
)

type memAppender struct {
	events []model.Event
}

func (a *memAppender) Append(_ context.Context, event model.Event) (model.Event, error) {
	event.Sequence = uint64(len(a.events) + 1)
	a.events = append(a.events, event)
	return event, nil
}

type memBus struct {
	commands []model.DeviceCommand
}

func (b *memBus) Publish(_ context.Context, command model.DeviceCommand) error {
	for index, current := range b.commands {
		if current.DeviceID == command.DeviceID && command.Generation < current.Generation {
			return model.ErrCommandStale
		}
		if current.DeviceID == command.DeviceID {
			b.commands[index] = command
			return nil
		}
	}
	b.commands = append(b.commands, command)
	return nil
}

func (b *memBus) find(deviceID string) (model.DeviceCommand, bool) {
	for _, command := range b.commands {
		if command.DeviceID == deviceID {
			return command, true
		}
	}
	return model.DeviceCommand{}, false
}

func newTestService() (*Service, *memAppender, *memBus, model.LineID, uuid.UUID, time.Time) {
	appender := &memAppender{}
	bus := &memBus{}
	service, _ := NewService(appender, bus, NewLedger(), NewRequestBook(), DefaultRiskPolicy())
	lineID := model.LineID("line-1")
	batchID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	now := time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)
	return service, appender, bus, lineID, batchID, now
}

func TestRiskPolicyClassification(t *testing.T) {
	policy := DefaultRiskPolicy()
	cases := []struct {
		name     string
		oldRate  float64
		newRate  float64
		highRisk bool
	}{
		{"首次小剂量直接投加", 0, 100, false},
		{"首次大剂量需审批", 0, 300, true},
		{"普通范围内上调", 100, 110, false},
		{"超过两成上调", 100, 121, true},
		{"超过两成下调同样审批", 100, 79, true},
		{"超过绝对上限", 400, 520, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := policy.IsHighRisk(tc.oldRate, tc.newRate); got != tc.highRisk {
				t.Fatalf("IsHighRisk(%v,%v)=%v want %v", tc.oldRate, tc.newRate, got, tc.highRisk)
			}
		})
	}
}

func TestNormalRangeChangeDispatchesImmediately(t *testing.T) {
	service, appender, bus, lineID, batchID, now := newTestService()
	submission, err := service.Submit(context.Background(), SubmitInput{
		LineID: lineID, BatchID: batchID, Chemical: "pac", Rate: 100, Generation: 1, RequestedBy: "op",
	}, now)
	if err != nil {
		t.Fatalf("submit normal change: %v", err)
	}
	if submission.HighRisk {
		t.Fatal("first low dose should be a normal change")
	}
	if len(appender.events) != 1 || appender.events[0].Kind != model.EventDosingChanged {
		t.Fatalf("normal change must journal dosing.changed, got %+v", appender.events)
	}
	command, ok := bus.find("doser-pac")
	if !ok || command.Setpoint != 100 {
		t.Fatalf("normal change must reach device bus immediately, got %+v", bus.commands)
	}
}

func TestHighRiskRequestHeldUntilApproved(t *testing.T) {
	service, appender, bus, lineID, batchID, now := newTestService()
	// 100 -> 400：相对增幅 300%，高风险。
	submission, err := service.Submit(context.Background(), SubmitInput{
		LineID: lineID, BatchID: batchID, Chemical: "pac", Rate: 100, Generation: 1, RequestedBy: "op",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	submission, err = service.Submit(context.Background(), SubmitInput{
		LineID: lineID, BatchID: batchID, Chemical: "pac", Rate: 400, Generation: 1,
		Reason: "upstream shock", RequestedBy: "op",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !submission.HighRisk || submission.Request == nil || submission.Request.Status != StatusPending {
		t.Fatalf("high risk change must create pending request, got %+v", submission)
	}
	if _, ok := bus.find("doser-pac"); ok {
		command, _ := bus.find("doser-pac")
		if command.Setpoint == 400 {
			t.Fatal("unapproved high risk change must never reach the device bus")
		}
	}
	if got := appender.events[len(appender.events)-1].Kind; got != model.EventDoseRequested {
		t.Fatalf("expected request_submitted event, got %s", got)
	}

	// 同药剂已有待审核申请时，再次提交必须被拒绝。
	if _, err := service.Submit(context.Background(), SubmitInput{
		LineID: lineID, BatchID: batchID, Chemical: "pac", Rate: 410, Generation: 1, RequestedBy: "op",
	}, now); err == nil {
		t.Fatal("duplicate pending request for the same chemical must be rejected")
	}

	// 驳回：总线仍不得出现 400。
	rejected, err := service.Review(context.Background(), submission.Request.ID, DecisionReject, "chief", "unsafe", now)
	if err != nil {
		t.Fatal(err)
	}
	if rejected.Request.Status != StatusRejected {
		t.Fatalf("expected rejected, got %s", rejected.Request.Status)
	}
	if rejected.Dispatch != nil {
		t.Fatal("rejected request must not carry a dispatch")
	}
	if command, ok := bus.find("doser-pac"); ok && command.Setpoint == 400 {
		t.Fatal("rejected change reached the bus")
	}
}

func TestApproveDispatchesAndRecordsOldAndNew(t *testing.T) {
	service, _, bus, lineID, batchID, now := newTestService()
	if _, err := service.Submit(context.Background(), SubmitInput{
		LineID: lineID, BatchID: batchID, Chemical: "pac", Rate: 100, Generation: 1, RequestedBy: "op",
	}, now); err != nil {
		t.Fatal(err)
	}
	submission, err := service.Submit(context.Background(), SubmitInput{
		LineID: lineID, BatchID: batchID, Chemical: "pac", Rate: 400, Generation: 1,
		Reason: "shock", RequestedBy: "op",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	record, err := service.Review(context.Background(), submission.Request.ID, DecisionApprove, "chief", "ok", now)
	if err != nil {
		t.Fatal(err)
	}
	if record.Request.Status != StatusApproved || record.Dispatch == nil {
		t.Fatalf("approval must dispatch, got %+v", record)
	}
	if record.Dispatch.Change.OldRate != 100 || record.Dispatch.Change.Rate != 400 {
		t.Fatalf("dispatch must carry old/new values, got %+v", record.Dispatch.Change)
	}
	command, ok := bus.find("doser-pac")
	if !ok || command.Setpoint != 400 {
		t.Fatalf("approved change must reach bus, got %+v", bus.commands)
	}
	// 已审核申请不能再次审核。
	if _, err := service.Review(context.Background(), submission.Request.ID, DecisionApprove, "chief", "", now); err == nil {
		t.Fatal("reviewing an already reviewed request must fail")
	}
	// 已审核申请不能撤回。
	if _, err := service.Cancel(context.Background(), submission.Request.ID, now); err == nil {
		t.Fatal("cancel after review must fail")
	}
}

func TestRevokeRestoresOldRateAndKeepsHistory(t *testing.T) {
	service, appender, bus, lineID, batchID, now := newTestService()
	if _, err := service.Submit(context.Background(), SubmitInput{
		LineID: lineID, BatchID: batchID, Chemical: "pac", Rate: 100, Generation: 1, RequestedBy: "op",
	}, now); err != nil {
		t.Fatal(err)
	}
	submission, _ := service.Submit(context.Background(), SubmitInput{
		LineID: lineID, BatchID: batchID, Chemical: "pac", Rate: 400, Generation: 1,
		Reason: "shock", RequestedBy: "op",
	}, now)
	if _, err := service.Review(context.Background(), submission.Request.ID, DecisionApprove, "chief", "ok", now); err != nil {
		t.Fatal(err)
	}

	record, err := service.Revoke(context.Background(), submission.Request.ID, 2, "chief", "lab regressed", now)
	if err != nil {
		t.Fatal(err)
	}
	if record.Request.Status != StatusRevoked {
		t.Fatalf("expected revoked, got %s", record.Request.Status)
	}
	if record.Compensation.Action != "set" || record.Compensation.Setpoint != 100 {
		t.Fatalf("compensation must restore old rate 100 via set, got %s/%v", record.Compensation.Action, record.Compensation.Setpoint)
	}
	command, _ := bus.find("doser-pac")
	if command.Action != "set" || command.Setpoint != 100 || command.Generation != 2 {
		t.Fatalf("bus must hold compensation command, got %+v", command)
	}
	history := service.History(batchID)
	if len(history) != 3 || !history[2].Compensation {
		t.Fatalf("history must retain original changes plus compensation, got %+v", history)
	}
	if got := appender.events[len(appender.events)-1].Kind; got != model.EventDoseRevoked {
		t.Fatalf("expected change_revoked event, got %s", got)
	}
	// 已撤销的申请不能再次撤销。
	if _, err := service.Revoke(context.Background(), submission.Request.ID, 3, "chief", "", now); err == nil {
		t.Fatal("double revoke must fail")
	}
}

func TestRevokeFirstDoseStopsDoser(t *testing.T) {
	service, _, bus, lineID, batchID, now := newTestService()
	// 首次投加 pam 300（超过绝对上限一半）即高风险。
	submission, err := service.Submit(context.Background(), SubmitInput{
		LineID: lineID, BatchID: batchID, Chemical: "pam", Rate: 300, Generation: 1,
		Reason: "startup", RequestedBy: "op",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !submission.HighRisk {
		t.Fatal("first dose above half absolute limit must be high risk")
	}
	if _, err := service.Review(context.Background(), submission.Request.ID, DecisionApprove, "chief", "ok", now); err != nil {
		t.Fatal(err)
	}
	record, err := service.Revoke(context.Background(), submission.Request.ID, 2, "chief", "stop for safety", now)
	if err != nil {
		t.Fatal(err)
	}
	if record.Compensation.Action != "stop" || record.Compensation.Setpoint != 0 {
		t.Fatalf("revoking first-ever dose must stop the doser, got %s/%v", record.Compensation.Action, record.Compensation.Setpoint)
	}
	command, _ := bus.find("doser-pam")
	if command.Action != "stop" {
		t.Fatalf("bus must hold stop command, got %+v", command)
	}
}

func TestReplayRebuildsStateAndToleratesStale(t *testing.T) {
	appender, bus := &memAppender{}, &memBus{}
	service, _ := NewService(appender, bus, NewLedger(), NewRequestBook(), DefaultRiskPolicy())
	lineID := model.LineID("line-1")
	batchID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	now := time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)

	if _, err := service.Submit(context.Background(), SubmitInput{
		LineID: lineID, BatchID: batchID, Chemical: "pac", Rate: 100, Generation: 1, RequestedBy: "op",
	}, now); err != nil {
		t.Fatal(err)
	}
	submission, _ := service.Submit(context.Background(), SubmitInput{
		LineID: lineID, BatchID: batchID, Chemical: "pac", Rate: 400, Generation: 1, RequestedBy: "op",
	}, now)
	if _, err := service.Review(context.Background(), submission.Request.ID, DecisionApprove, "chief", "ok", now); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Revoke(context.Background(), submission.Request.ID, 2, "chief", "", now); err != nil {
		t.Fatal(err)
	}

	// 用同一事件流、全新内存状态恢复。
	recovered, _ := NewService(appender, &memBus{}, NewLedger(), NewRequestBook(), DefaultRiskPolicy())
	for _, event := range appender.events {
		if err := recovered.ReplayEvent(context.Background(), event); err != nil {
			t.Fatalf("replay %s: %v", event.Kind, err)
		}
	}
	request, ok := recovered.GetRequest(submission.Request.ID)
	if !ok || request.Status != StatusRevoked {
		t.Fatalf("recovered request must be revoked, got %+v", request)
	}
	current := recovered.Current(batchID)
	if len(current) != 1 || current[0].Rate != 100 || !current[0].Compensation {
		t.Fatalf("recovered ledger must end at compensation rate 100, got %+v", current)
	}

	// 代际已被推进（急停）时，重放旧命令必须被容忍而不是中断恢复。
	staleBus := &memBus{}
	stopCommand, _ := model.NewDeviceCommand(lineID, batchID, "doser-pac", model.DeviceDoser, 9, "stop", now)
	if err := staleBus.Publish(context.Background(), stopCommand); err != nil {
		t.Fatal(err)
	}
	tolerant, _ := NewService(appender, staleBus, NewLedger(), NewRequestBook(), DefaultRiskPolicy())
	for _, event := range appender.events {
		if err := tolerant.ReplayEvent(context.Background(), event); err != nil {
			t.Fatalf("stale replay must be tolerated, got %v on %s", err, event.Kind)
		}
	}
}

func TestRequestBookRejectsStatusRegression(t *testing.T) {
	book := NewRequestBook()
	now := time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)
	lineID := model.LineID("line-1")
	batchID := uuid.New()
	request, err := NewChangeRequest(lineID, batchID, "pac", 100, 400, 1, true, "", "op", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := book.Save(request); err != nil {
		t.Fatal(err)
	}
	approved, err := request.Decide(DecisionApprove, "chief", "", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := book.Save(approved); err != nil {
		t.Fatal(err)
	}
	// 旧的 pending 事件晚到，不得覆盖已批准状态。
	if err := book.Save(request); err == nil {
		t.Fatal("regressing request status via stale event must be rejected")
	}
}
