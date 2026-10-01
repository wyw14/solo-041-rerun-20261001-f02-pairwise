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

func (p *Plant) SubmitAeration(ctx context.Context, lineID model.LineID, basinID string, payload []byte) (aeration.Window, *model.DeviceCommand, error) {
	p.mu.RLock()
	state, ok := p.lines[lineID]
	p.mu.RUnlock()
	if !ok || state.Emergency {
		return aeration.Window{}, nil, errors.New("process line cannot accept aeration samples")
	}
	return p.aeration.SubmitWindow(ctx, lineID, basinID, payload, state.Generation, p.now())
}

func (p *Plant) ChangeDose(ctx context.Context, lineID model.LineID, batchID uuid.UUID, chemical string, rate float64, requestedBy string) (dosing.Outcome, error) {
	p.mu.RLock()
	state, ok := p.lines[lineID]
	p.mu.RUnlock()
	if !ok || state.Emergency {
		return dosing.Outcome{}, errors.New("process line cannot change dosing")
	}
	outcome, err := p.dosing.Submit(ctx, dosing.SubmitInput{
		LineID: lineID, BatchID: batchID, Chemical: chemical, Rate: rate,
		Generation: state.Generation, RequestedBy: requestedBy,
	}, p.now())
	if err != nil {
		return dosing.Outcome{}, err
	}
	p.touchLine(lineID)
	return outcome, nil
}

func (p *Plant) ApproveDoseChange(ctx context.Context, requestID uuid.UUID, reviewer, note string) (dosing.Request, dosing.Change, model.DeviceCommand, error) {
	request, err := p.doseRequestForAction(requestID)
	if err != nil {
		return dosing.Request{}, dosing.Change{}, model.DeviceCommand{}, err
	}
	p.mu.RLock()
	state := p.lines[request.LineID]
	p.mu.RUnlock()
	if state.Emergency || state.Generation != request.Generation {
		return dosing.Request{}, dosing.Change{}, model.DeviceCommand{}, errors.New("process line state changed; dose request must be resubmitted")
	}
	approved, change, command, err := p.dosing.Approve(ctx, requestID, reviewer, note, p.now())
	if err != nil {
		return dosing.Request{}, dosing.Change{}, model.DeviceCommand{}, err
	}
	p.touchLine(request.LineID)
	return approved, change, command, nil
}

func (p *Plant) RejectDoseChange(ctx context.Context, requestID uuid.UUID, reviewer, note string) (dosing.Request, error) {
	if _, err := p.doseRequestForAction(requestID); err != nil {
		return dosing.Request{}, err
	}
	rejected, err := p.dosing.Reject(ctx, requestID, reviewer, note, p.now())
	if err != nil {
		return dosing.Request{}, err
	}
	p.touchLine(rejected.LineID)
	return rejected, nil
}

func (p *Plant) RevokeDoseChange(ctx context.Context, requestID uuid.UUID, operator string) (dosing.Request, *model.DeviceCommand, error) {
	request, err := p.doseRequestForAction(requestID)
	if err != nil {
		return dosing.Request{}, nil, err
	}
	p.mu.RLock()
	state := p.lines[request.LineID]
	p.mu.RUnlock()
	// 已下发命令若已被急停等高代际动作取代，总线回退会被拒绝；此时安全状态由急停保证，
	// 不允许再撤销该命令，避免产生误导性的“已回退”历史。
	if request.Issued() && (state.Emergency || state.Generation != request.Generation) {
		return dosing.Request{}, nil, errors.New("process line state changed; issued dose is superseded and cannot be revoked")
	}
	revoked, command, err := p.dosing.Revoke(ctx, requestID, operator, p.now())
	if err != nil {
		return dosing.Request{}, nil, err
	}
	p.touchLine(revoked.LineID)
	return revoked, command, nil
}

func (p *Plant) doseRequestForAction(requestID uuid.UUID) (dosing.Request, error) {
	request, ok := p.dosing.GetRequest(requestID)
	if !ok {
		return dosing.Request{}, errors.New("dose change request is not found")
	}
	return request, nil
}

func (p *Plant) touchLine(lineID model.LineID) {
	p.mu.Lock()
	state, ok := p.lines[lineID]
	if ok {
		state.UpdatedAt = p.now().UTC()
		p.lines[lineID] = state
	}
	p.mu.Unlock()
}

func (p *Plant) PendingDoseRequests() []dosing.Request {
	return p.dosing.PendingRequests("")
}

func (p *Plant) DoseRequests() []dosing.Request {
	return p.dosing.AllRequests()
}

func (p *Plant) DoseHistory(batchID uuid.UUID) []dosing.Change {
	return p.dosing.History(batchID)
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
