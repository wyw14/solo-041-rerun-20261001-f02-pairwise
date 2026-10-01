package dosing

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/journal"
	"github.com/wyw14/cry-97/internal/model"
)

type CommandPublisher interface {
	Publish(context.Context, model.DeviceCommand) error
}

const (
	actionSet  = "set"
	actionStop = "stop"
)

// commandNamespace 为同一变更生成确定性命令 ID，保证事件重放后总线命令一致。
var commandNamespace = uuid.NameSpaceOID

// PublishError 表示设备总线拒绝命令（此时事件尚未落盘，调用方可安全重试或改走申请）。
type PublishError struct {
	Stage string
	Err   error
}

func (e *PublishError) Error() string {
	return fmt.Sprintf("device bus rejected dosing command at %s: %v", e.Stage, e.Err)
}

func (e *PublishError) Unwrap() error { return e.Err }

// Submission 是提交加药变更后的结果：普通范围内变更直接下发；高风险变更只生成待审核申请。
type Submission struct {
	HighRisk bool                 `json:"high_risk"`
	Request  *ChangeRequest       `json:"request,omitempty"`
	Change   *Change              `json:"change,omitempty"`
	Command  *model.DeviceCommand `json:"command,omitempty"`
}

// ReviewRecord 是审核事件的落盘载荷；批准时携带实际下发的变更与命令。
type ReviewRecord struct {
	Request  ChangeRequest `json:"request"`
	Dispatch *Dispatch     `json:"dispatch,omitempty"`
}

// Dispatch 是一次批准后进入设备总线的加药下发。
type Dispatch struct {
	Change  Change              `json:"change"`
	Command model.DeviceCommand `json:"command"`
}

// RevocationRecord 是撤销事件的落盘载荷，包含安全补偿命令。
type RevocationRecord struct {
	Request            ChangeRequest       `json:"request"`
	Change             Change              `json:"change"`
	CompensationChange Change              `json:"compensation_change"`
	Compensation       model.DeviceCommand `json:"compensation"`
}

type Service struct {
	journal   journal.Appender
	publisher CommandPublisher
	ledger    *Ledger
	requests  *RequestBook
	policy    RiskPolicy

	mu sync.Mutex
}

func NewService(appender journal.Appender, publisher CommandPublisher, ledger *Ledger, book *RequestBook, policy RiskPolicy) (*Service, error) {
	if appender == nil || publisher == nil || ledger == nil || book == nil {
		return nil, errors.New("dosing service dependencies are required")
	}
	return &Service{
		journal: appender, publisher: publisher, ledger: ledger,
		requests: book, policy: policy.normalized(),
	}, nil
}

// SubmitInput 是操作员提交的加药变更。
type SubmitInput struct {
	LineID      model.LineID
	BatchID     uuid.UUID
	Chemical    string
	Rate        float64
	Generation  uint64
	Reason      string
	RequestedBy string
}

// Submit 判定风险等级：普通范围内变更直接发布命令并记事件；高风险变更只生成待审核申请，
// 在批准前不会向设备总线发布任何命令。
func (s *Service) Submit(ctx context.Context, input SubmitInput, now time.Time) (Submission, error) {
	if err := ctx.Err(); err != nil {
		return Submission{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var oldRate float64
	if previous, found := s.ledger.LatestForChemical(input.BatchID, input.Chemical); found {
		oldRate = previous.Rate
	}
	if s.policy.IsHighRisk(oldRate, input.Rate) {
		if pending, exists := s.requests.PendingFor(input.BatchID, input.Chemical); exists {
			return Submission{}, fmt.Errorf("chemical %s already has pending dosing request %s", input.Chemical, pending.ID)
		}
		request, err := NewChangeRequest(input.LineID, input.BatchID, input.Chemical, oldRate, input.Rate,
			input.Generation, true, input.Reason, input.RequestedBy, now)
		if err != nil {
			return Submission{}, err
		}
		if _, err := s.appendEvent(ctx, model.EventDoseRequested, request, now); err != nil {
			return Submission{}, err
		}
		if err := s.requests.Save(request); err != nil {
			return Submission{}, err
		}
		return Submission{HighRisk: true, Request: &request}, nil
	}

	change, err := NewChange(input.LineID, input.BatchID, input.Chemical, oldRate, input.Rate,
		input.Generation, false, uuid.Nil, now)
	if err != nil {
		return Submission{}, err
	}
	command, err := s.publishThenRecord(ctx, model.EventDosingChanged, change, now, nil)
	if err != nil {
		return Submission{}, err
	}
	return Submission{HighRisk: false, Change: &change, Command: command}, nil
}

// Review 处理高风险申请。驳回只记录审核结果；批准后才把设定值命令发布到设备总线。
func (s *Service) Review(ctx context.Context, requestID uuid.UUID, decision ReviewDecision, reviewer, note string, now time.Time) (ReviewRecord, error) {
	if err := ctx.Err(); err != nil {
		return ReviewRecord{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	request, ok := s.requests.Get(requestID)
	if !ok {
		return ReviewRecord{}, errors.New("dosing request is not found")
	}
	reviewed, err := request.Decide(decision, reviewer, note, now)
	if err != nil {
		return ReviewRecord{}, err
	}
	record := ReviewRecord{Request: reviewed}
	if reviewed.Status != StatusApproved {
		if _, err := s.appendEvent(ctx, model.EventDoseReviewed, record, now); err != nil {
			return ReviewRecord{}, err
		}
		if err := s.requests.Save(reviewed); err != nil {
			return ReviewRecord{}, err
		}
		return record, nil
	}

	change, err := NewChange(reviewed.LineID, reviewed.BatchID, reviewed.Chemical, reviewed.OldRate, reviewed.NewRate,
		reviewed.Generation, true, reviewed.ID, now)
	if err != nil {
		return ReviewRecord{}, err
	}
	command, err := s.buildCommand(change, now)
	if err != nil {
		return ReviewRecord{}, err
	}
	// 先发布总线：被拒绝时申请仍保持待审核，操作员可重试批准。
	if err := s.publisher.Publish(ctx, command); err != nil {
		return ReviewRecord{}, &PublishError{Stage: "review", Err: err}
	}
	linked, err := reviewed.MarkDispatched(change.ID)
	if err != nil {
		return ReviewRecord{}, err
	}
	reviewed = linked
	record = ReviewRecord{Request: reviewed, Dispatch: &Dispatch{Change: change, Command: command}}
	if _, err := s.appendEvent(ctx, model.EventDoseReviewed, record, now); err != nil {
		return ReviewRecord{}, err
	}
	if err := s.requests.Save(reviewed); err != nil {
		return ReviewRecord{}, err
	}
	s.ledger.Record(change)
	return record, nil
}

// Cancel 允许申请人在审核前撤回尚未批准的申请。
func (s *Service) Cancel(ctx context.Context, requestID uuid.UUID, now time.Time) (ChangeRequest, error) {
	if err := ctx.Err(); err != nil {
		return ChangeRequest{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	request, ok := s.requests.Get(requestID)
	if !ok {
		return ChangeRequest{}, errors.New("dosing request is not found")
	}
	canceled, err := request.Cancel(now)
	if err != nil {
		return ChangeRequest{}, err
	}
	if _, err := s.appendEvent(ctx, model.EventDoseCanceled, canceled, now); err != nil {
		return ChangeRequest{}, err
	}
	if err := s.requests.Save(canceled); err != nil {
		return ChangeRequest{}, err
	}
	return canceled, nil
}

// Revoke 撤销已下发的批准变更，并立即生成安全补偿命令：此前有设定值则恢复旧值，
// 此前从未投加则下发 stop；申请、原变更与补偿全程保留在台账和事件历史中。
func (s *Service) Revoke(ctx context.Context, requestID uuid.UUID, generation uint64, operator, note string, now time.Time) (RevocationRecord, error) {
	if err := ctx.Err(); err != nil {
		return RevocationRecord{}, err
	}
	if generation == 0 {
		return RevocationRecord{}, errors.New("revocation generation is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	request, ok := s.requests.Get(requestID)
	if !ok {
		return RevocationRecord{}, errors.New("dosing request is not found")
	}
	change, ok := s.ledger.Find(request.BatchID, request.ChangeID)
	if !ok {
		return RevocationRecord{}, errors.New("dispatched dosing change is not found")
	}

	compensationChange := NewCompensationChange(change, generation, now)
	command, err := s.buildCommand(compensationChange, now)
	if err != nil {
		return RevocationRecord{}, err
	}
	if change.OldRate > 0 {
		command.Action = actionSet
		command.Setpoint = change.OldRate
	} else {
		command.Action = actionStop
		command.Setpoint = 0
	}
	command.Metadata["compensates_change"] = change.ID.String()
	command.Metadata["revoked_request"] = request.ID.String()

	revoked, err := request.Revoke(operator, note, command.ID, now)
	if err != nil {
		return RevocationRecord{}, err
	}
	// 先发布补偿命令：总线拒绝时申请仍为已批准，操作员可重试撤销。
	if err := s.publisher.Publish(ctx, command); err != nil {
		return RevocationRecord{}, &PublishError{Stage: "revoke", Err: err}
	}
	record := RevocationRecord{Request: revoked, Change: change, CompensationChange: compensationChange, Compensation: command}
	if _, err := s.appendEvent(ctx, model.EventDoseRevoked, record, now); err != nil {
		return RevocationRecord{}, err
	}
	if err := s.requests.Save(revoked); err != nil {
		return RevocationRecord{}, err
	}
	s.ledger.Record(compensationChange)
	return record, nil
}

func (s *Service) Latest(batchID uuid.UUID) (Change, bool) {
	return s.ledger.Latest(batchID)
}

func (s *Service) LatestForChemical(batchID uuid.UUID, chemical string) (Change, bool) {
	return s.ledger.LatestForChemical(batchID, chemical)
}

func (s *Service) History(batchID uuid.UUID) []Change {
	return s.ledger.History(batchID)
}

func (s *Service) Current(batchID uuid.UUID) []Change {
	return s.ledger.Current(batchID)
}

func (s *Service) GetRequest(id uuid.UUID) (ChangeRequest, bool) {
	return s.requests.Get(id)
}

func (s *Service) ListRequests(filter RequestStatus) []ChangeRequest {
	return s.requests.List(filter)
}

func (s *Service) appendEvent(ctx context.Context, kind model.EventKind, payload any, now time.Time) (model.Event, error) {
	lineID, generation := eventIdentity(payload)
	event, err := model.NewEvent(lineID, generation, kind, payload, now)
	if err != nil {
		return model.Event{}, err
	}
	appended, err := s.journal.Append(ctx, event)
	if err != nil {
		return model.Event{}, fmt.Errorf("append dosing event %s: %w", kind, err)
	}
	return appended, nil
}

// publishThenRecord 用于普通范围内变更：先发布总线，成功后记事件和台账。
func (s *Service) publishThenRecord(ctx context.Context, kind model.EventKind, change Change, now time.Time, metadata map[string]string) (*model.DeviceCommand, error) {
	command, err := s.buildCommand(change, now)
	if err != nil {
		return nil, err
	}
	for key, value := range metadata {
		command.Metadata[key] = value
	}
	if err := s.publisher.Publish(ctx, command); err != nil {
		return nil, &PublishError{Stage: "submit", Err: err}
	}
	if _, err := s.appendEvent(ctx, kind, change, now); err != nil {
		return nil, err
	}
	s.ledger.Record(change)
	return &command, nil
}

func (s *Service) buildCommand(change Change, now time.Time) (model.DeviceCommand, error) {
	key := fmt.Sprintf("dose-command:%s:%s", change.Chemical, change.ID)
	command, err := model.NewDeviceCommand(change.LineID, change.BatchID, "doser-"+change.Chemical,
		model.DeviceDoser, change.Generation, actionSet, now)
	if err != nil {
		return model.DeviceCommand{}, err
	}
	command.ID = uuid.NewSHA1(commandNamespace, []byte(key))
	command.Setpoint = change.Rate
	command.Metadata["change_id"] = change.ID.String()
	if change.RequestID != uuid.Nil {
		command.Metadata["request_id"] = change.RequestID.String()
	}
	return command, nil
}

func eventIdentity(payload any) (model.LineID, uint64) {
	switch value := payload.(type) {
	case Change:
		return value.LineID, value.Generation
	case ChangeRequest:
		return value.LineID, value.Generation
	case ReviewRecord:
		return value.Request.LineID, value.Request.Generation
	case RevocationRecord:
		return value.Request.LineID, value.CompensationChange.Generation
	default:
		return "", 0
	}
}
