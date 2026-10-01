package process

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/aeration"
	"github.com/wyw14/cry-97/internal/dosing"
	"github.com/wyw14/cry-97/internal/model"
	"github.com/wyw14/cry-97/internal/settling"
)

// ErrDoseRequestNotFound 表示高风险加药申请不存在，供 API 层映射为 404。
var ErrDoseRequestNotFound = errors.New("dosing request is not found")

func (p *Plant) SubmitAeration(ctx context.Context, lineID model.LineID, basinID string, payload []byte) (aeration.Window, *model.DeviceCommand, error) {
	p.mu.RLock()
	state, ok := p.lines[lineID]
	p.mu.RUnlock()
	if !ok || state.Emergency {
		return aeration.Window{}, nil, errors.New("process line cannot accept aeration samples")
	}
	return p.aeration.SubmitWindow(ctx, lineID, basinID, payload, state.Generation, p.now())
}

// SubmitDoseChange 提交加药剂量变更：普通范围内变更直接下发，高风险变更进入待审核状态，
// 在审核通过前不会向设备总线发布命令。
func (p *Plant) SubmitDoseChange(ctx context.Context, lineID model.LineID, batchID uuid.UUID, chemical string, rate float64, reason, requestedBy string) (dosing.Submission, error) {
	p.mu.RLock()
	state, ok := p.lines[lineID]
	p.mu.RUnlock()
	if !ok || state.Emergency {
		return dosing.Submission{}, errors.New("process line cannot change dosing")
	}
	if err := p.verifyDoseBatch(lineID, batchID); err != nil {
		return dosing.Submission{}, err
	}
	return p.dosing.Submit(ctx, dosing.SubmitInput{
		LineID: lineID, BatchID: batchID, Chemical: chemical, Rate: rate,
		Generation: state.Generation, Reason: reason, RequestedBy: requestedBy,
	}, p.now())
}

// ReviewDoseChange 审核高风险加药申请。批准时代际必须仍然有效（处理线未被急停推进）。
func (p *Plant) ReviewDoseChange(ctx context.Context, requestID uuid.UUID, decision dosing.ReviewDecision, reviewer, note string) (dosing.ReviewRecord, error) {
	request, ok := p.dosing.GetRequest(requestID)
	if !ok {
		return dosing.ReviewRecord{}, ErrDoseRequestNotFound
	}
	p.mu.RLock()
	state, ok := p.lines[request.LineID]
	p.mu.RUnlock()
	if !ok || state.Emergency {
		return dosing.ReviewRecord{}, errors.New("process line cannot accept dosing review")
	}
	if err := p.verifyDoseBatch(request.LineID, request.BatchID); err != nil {
		return dosing.ReviewRecord{}, err
	}
	if decision == dosing.DecisionApprove && request.Generation != state.Generation {
		return dosing.ReviewRecord{}, errors.New("dosing request generation is stale; resubmit required")
	}
	return p.dosing.Review(ctx, requestID, decision, reviewer, note, p.now())
}

// CancelDoseChange 撤回尚未审核的高风险申请。
func (p *Plant) CancelDoseChange(ctx context.Context, requestID uuid.UUID) (dosing.ChangeRequest, error) {
	if _, ok := p.dosing.GetRequest(requestID); !ok {
		return dosing.ChangeRequest{}, ErrDoseRequestNotFound
	}
	return p.dosing.Cancel(ctx, requestID, p.now())
}

// RevokeDoseChange 撤销已下发的批准变更，向设备总线发布恢复旧值（或停机）的安全补偿命令。
func (p *Plant) RevokeDoseChange(ctx context.Context, requestID uuid.UUID, operator, note string) (dosing.RevocationRecord, error) {
	request, ok := p.dosing.GetRequest(requestID)
	if !ok {
		return dosing.RevocationRecord{}, ErrDoseRequestNotFound
	}
	p.mu.RLock()
	state, ok := p.lines[request.LineID]
	p.mu.RUnlock()
	if !ok {
		return dosing.RevocationRecord{}, errors.New("process line is not found")
	}
	if err := p.verifyDoseBatch(request.LineID, request.BatchID); err != nil {
		return dosing.RevocationRecord{}, err
	}
	return p.dosing.Revoke(ctx, requestID, state.Generation, operator, note, p.now())
}

func (p *Plant) verifyDoseBatch(lineID model.LineID, batchID uuid.UUID) error {
	batch, ok := p.ActiveBatch(lineID)
	if !ok {
		return errors.New("process line has no active batch")
	}
	if batch.ID != batchID {
		return errors.New("dosing batch is not the active batch of the line")
	}
	return nil
}

func (p *Plant) DoseRequests(filter dosing.RequestStatus) []dosing.ChangeRequest {
	return p.dosing.ListRequests(filter)
}

func (p *Plant) DoseRequest(id uuid.UUID) (dosing.ChangeRequest, bool) {
	return p.dosing.GetRequest(id)
}

func (p *Plant) UpdateBlanket(lineID model.LineID, level float64) (settling.Snapshot, error) {
	state, ok := p.settling[lineID]
	if !ok {
		return settling.Snapshot{}, errors.New("settling line is not found")
	}
	return state.UpdateBlanket(level, 2.8, p.now()), nil
}

func (p *Plant) ReceiveSample(ctx context.Context, sample model.Sample) error {
	if err := p.receiver.Receive(ctx, sample, p.now()); err != nil {
		return err
	}
	return p.sampler.Publish(sample)
}

func (p *Plant) BeginSettling(lineID model.LineID) (settling.Cycle, error) {
	state, ok := p.settling[lineID]
	if !ok {
		return settling.Cycle{}, errors.New("settling line is not found")
	}
	state.BeginCycle(p.now())
	return p.cycles.Start(lineID, p.now())
}

func (p *Plant) AdvanceSettling(lineID model.LineID) (settling.Cycle, error) {
	return p.cycles.Advance(lineID, p.now())
}

func (p *Plant) Alarms() []model.Alarm {
	return p.alarms.All()
}

func (p *Plant) StatusBlowers() []aeration.Blower {
	return p.aeration.Blowers()
}

func (p *Plant) StatusAerationWindows() []aeration.Window {
	return p.aeration.Windows()
}

func (p *Plant) AcknowledgeAlarm(id uuid.UUID) (model.Alarm, error) {
	return p.alarms.Acknowledge(id)
}

func (p *Plant) RecoverAlarm(id uuid.UUID) (model.Alarm, error) {
	return p.alarms.Recover(id, p.now())
}

func (p *Plant) Clock() time.Time {
	return p.now().UTC()
}
