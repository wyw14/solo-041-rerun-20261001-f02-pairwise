package dosing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/journal"
	"github.com/wyw14/cry-97/internal/model"
)

type CommandPublisher interface {
	Publish(context.Context, model.DeviceCommand) error
}

// CompensationScheduler 接收撤销已下发变更时生成的安全补偿命令。
type CompensationScheduler interface {
	Schedule(journal.Compensation)
}

// SubmitInput 是一次加药速率调整请求。
type SubmitInput struct {
	LineID      model.LineID
	BatchID     uuid.UUID
	Chemical    string
	Rate        float64
	Generation  uint64
	RequestedBy string
}

// Outcome 描述加药请求的处理结果：普通变更直接携带命令，高风险变更携带待审申请。
type Outcome struct {
	Risk    RiskLevel            `json:"risk"`
	Change  *Change              `json:"change,omitempty"`
	Command *model.DeviceCommand `json:"command,omitempty"`
	Request *Request             `json:"request,omitempty"`
}

type Service struct {
	journal       journal.Appender
	publisher     CommandPublisher
	ledger        *Ledger
	requests      *RequestBook
	compensations CompensationScheduler
	policy        RiskPolicy
}

func NewService(appender journal.Appender, publisher CommandPublisher, ledger *Ledger) (*Service, error) {
	if appender == nil || publisher == nil || ledger == nil {
		return nil, errors.New("dosing service dependencies are required")
	}
	return &Service{
		journal: appender, publisher: publisher, ledger: ledger,
		requests: NewRequestBook(), policy: DefaultRiskPolicy(),
	}, nil
}

// WithCompensations 挂载安全补偿队列，撤销已下发变更时向其登记补偿命令。
func (s *Service) WithCompensations(queue CompensationScheduler) *Service {
	s.compensations = queue
	return s
}

// WithPolicy 替换高风险判定策略（主要用于测试与按药剂调参）。
func (s *Service) WithPolicy(policy RiskPolicy) *Service {
	s.policy = policy
	return s
}

// Submit 判定风险并分流：普通范围变更直接记事件、下发命令；高风险变更只生成待审申请，
// 在批准前不会产生任何设备总线命令。
func (s *Service) Submit(ctx context.Context, input SubmitInput, now time.Time) (Outcome, error) {
	if err := ctx.Err(); err != nil {
		return Outcome{}, err
	}
	if input.RequestedBy == "" {
		return Outcome{}, errors.New("dose change applicant is required")
	}
	previous, _ := s.ledger.Current(input.BatchID, input.Chemical)
	risk, reasons := s.policy.Classify(previous.Rate, input.Rate)
	if risk == RiskNormal {
		change, command, err := s.applyChange(ctx, input.LineID, input.BatchID, input.Chemical, previous.Rate, input.Rate, input.Generation, uuid.Nil, now)
		if err != nil {
			return Outcome{}, err
		}
		return Outcome{Risk: RiskNormal, Change: &change, Command: &command}, nil
	}
	request, err := newRequest(input.LineID, input.BatchID, input.Chemical, previous.Rate, input.Rate, input.Generation, input.RequestedBy, reasons, now)
	if err != nil {
		return Outcome{}, err
	}
	if err := s.recordEvent(ctx, input.LineID, input.Generation, model.EventDoseRequested, request, now); err != nil {
		return Outcome{}, fmt.Errorf("append dose request failed: %w", err)
	}
	s.requests.Put(request)
	return Outcome{Risk: RiskHigh, Request: &request}, nil
}

// Approve 批准高风险变更：变更事件先入日志、命令随后进入设备总线，最后持久化携带 CommandID 的审批记录。
func (s *Service) Approve(ctx context.Context, requestID uuid.UUID, reviewer, note string, now time.Time) (Request, Change, model.DeviceCommand, error) {
	request, err := s.requests.MustGet(requestID)
	if err != nil {
		return Request{}, Change{}, model.DeviceCommand{}, err
	}
	request, err = request.decide(StatusApproved, reviewer, note, now)
	if err != nil {
		return Request{}, Change{}, model.DeviceCommand{}, err
	}
	change, command, err := s.applyChange(ctx, request.LineID, request.BatchID, request.Chemical, request.PreviousRate, request.Rate, request.Generation, request.ID, now)
	if err != nil {
		return Request{}, Change{}, model.DeviceCommand{}, err
	}
	request.CommandID = &command.ID
	if err := s.recordEvent(ctx, request.LineID, request.Generation, model.EventDoseApproved, request, now); err != nil {
		return Request{}, Change{}, model.DeviceCommand{}, fmt.Errorf("append dose approval failed: %w", err)
	}
	s.requests.Put(request)
	return request, change, command, nil
}

// Reject 拒绝高风险变更：变更从未进入设备总线，仅保留审批历史。
func (s *Service) Reject(ctx context.Context, requestID uuid.UUID, reviewer, note string, now time.Time) (Request, error) {
	request, err := s.requests.MustGet(requestID)
	if err != nil {
		return Request{}, err
	}
	request, err = request.decide(StatusRejected, reviewer, note, now)
	if err != nil {
		return Request{}, err
	}
	if err := s.recordEvent(ctx, request.LineID, request.Generation, model.EventDoseRejected, request, now); err != nil {
		return Request{}, fmt.Errorf("append dose rejection failed: %w", err)
	}
	s.requests.Put(request)
	return request, nil
}

// Revoke 撤销变更。待审核撤销只关闭申请单；已下发撤销会把投加量回退到旧值，
// 立即下发回退命令并登记一条安全补偿命令，全部过程记入日志与台账历史。
func (s *Service) Revoke(ctx context.Context, requestID uuid.UUID, operator string, now time.Time) (Request, *model.DeviceCommand, error) {
	request, err := s.requests.MustGet(requestID)
	if err != nil {
		return Request{}, nil, err
	}
	wasIssued := request.Issued()
	request, err = request.revoke(operator, now)
	if err != nil {
		return Request{}, nil, err
	}
	var revertCommand *model.DeviceCommand
	if wasIssued {
		compensation, compErr := journal.NewCompensation(request.LineID, request.ID, request.Generation, s.deviceID(request.Chemical), "revert", now.Add(time.Second))
		if compErr != nil {
			return Request{}, nil, compErr
		}
		request.CompensationID = &compensation.ID
		// 先把投加量回退到旧值（事件 + 总线命令 + 台账），保证装置尽快回到安全状态。
		_, command, applyErr := s.applyRevert(ctx, request, now)
		if applyErr != nil {
			return Request{}, nil, applyErr
		}
		revertCommand = &command
		// 补偿队列兜底：即使即时回退命令丢失，维护任务也会再次下发旧值。
		if s.compensations != nil {
			s.compensations.Schedule(compensation)
		}
	}
	if err := s.recordEvent(ctx, request.LineID, request.Generation, model.EventDoseRevoked, request, now); err != nil {
		return Request{}, nil, fmt.Errorf("append dose revocation failed: %w", err)
	}
	s.requests.Put(request)
	return request, revertCommand, nil
}

// ExecuteCompensation 由补偿队列维护任务调用，重新下发撤销回退命令，保证总线最终安全。
func (s *Service) ExecuteCompensation(ctx context.Context, compensation journal.Compensation) error {
	if compensation.Action != "revert" {
		return errors.New("unsupported dosing compensation action")
	}
	request, ok := s.requests.Get(compensation.SequenceID)
	if !ok {
		return errors.New("dosing compensation request is not found")
	}
	command, err := s.buildCommand(request.LineID, request.BatchID, request.Chemical, request.Generation, "revert", request.PreviousRate, request.ID, uuid.Nil, compensation.DueAt)
	if err != nil {
		return err
	}
	return s.publisher.Publish(ctx, command)
}

// Restore 依据日志事件重建台账与申请单；dosing.changed 对应确实下发过的命令，
// 会重新发布到设备总线，而 pending/rejected 的申请没有任何 changed 事件，因此永远不会进总线。
func (s *Service) Restore(ctx context.Context, event model.Event) error {
	switch event.Kind {
	case model.EventDoseRequested:
		var request Request
		if err := event.Decode(&request); err != nil {
			return err
		}
		s.requests.Put(request)
	case model.EventDoseApproved, model.EventDoseRejected, model.EventDoseRevoked:
		var request Request
		if err := event.Decode(&request); err != nil {
			return err
		}
		s.requests.Put(request)
	case model.EventDosingChanged:
		var change Change
		if err := event.Decode(&change); err != nil {
			return err
		}
		command, err := s.buildCommand(change.LineID, change.BatchID, change.Chemical, change.Generation, "set", change.Rate, change.RequestID, change.ID, change.AppliedAt)
		if err != nil {
			return err
		}
		if change.Reason != "" {
			command.Action = "revert"
		}
		if err := s.publisher.Publish(ctx, command); err != nil {
			if errors.Is(err, model.ErrStaleCommand) {
				// 处理线已被急停隔离或命令已被更高代际取代：旧命令不再进入总线。
				return nil
			}
			return err
		}
		s.ledger.Record(change)
	}
	return nil
}

func (s *Service) applyChange(ctx context.Context, lineID model.LineID, batchID uuid.UUID, chemical string, previousRate, rate float64, generation uint64, requestID uuid.UUID, now time.Time) (Change, model.DeviceCommand, error) {
	change, err := NewChange(lineID, batchID, chemical, previousRate, rate, generation, now)
	if err != nil {
		return Change{}, model.DeviceCommand{}, err
	}
	change.RequestID = requestID
	if err := s.recordEvent(ctx, lineID, generation, model.EventDosingChanged, change, now); err != nil {
		return Change{}, model.DeviceCommand{}, fmt.Errorf("append dosing change failed: %w", err)
	}
	command, err := s.buildCommand(lineID, batchID, chemical, generation, "set", rate, requestID, change.ID, now)
	if err != nil {
		return Change{}, model.DeviceCommand{}, err
	}
	if err := s.publisher.Publish(ctx, command); err != nil {
		return Change{}, model.DeviceCommand{}, fmt.Errorf("publish dosing command: %w", err)
	}
	s.ledger.Record(change)
	return change, command, nil
}

func (s *Service) applyRevert(ctx context.Context, request Request, now time.Time) (Change, model.DeviceCommand, error) {
	change, err := NewChange(request.LineID, request.BatchID, request.Chemical, request.Rate, request.PreviousRate, request.Generation, now)
	if err != nil {
		return Change{}, model.DeviceCommand{}, err
	}
	change.RequestID = request.ID
	change.Reason = "revoked:" + request.ID.String()
	if err := s.recordEvent(ctx, request.LineID, request.Generation, model.EventDosingChanged, change, now); err != nil {
		return Change{}, model.DeviceCommand{}, fmt.Errorf("append dosing revert failed: %w", err)
	}
	command, err := s.buildCommand(request.LineID, request.BatchID, request.Chemical, request.Generation, "revert", request.PreviousRate, request.ID, change.ID, now)
	if err != nil {
		return Change{}, model.DeviceCommand{}, err
	}
	if err := s.publisher.Publish(ctx, command); err != nil {
		return Change{}, model.DeviceCommand{}, fmt.Errorf("publish dosing revert command: %w", err)
	}
	s.ledger.Record(change)
	return change, command, nil
}

func (s *Service) buildCommand(lineID model.LineID, batchID uuid.UUID, chemical string, generation uint64, action string, setpoint float64, requestID, changeID uuid.UUID, now time.Time) (model.DeviceCommand, error) {
	command, err := model.NewDeviceCommand(lineID, batchID, s.deviceID(chemical), model.DeviceDoser, generation, action, now)
	if err != nil {
		return model.DeviceCommand{}, err
	}
	command.Setpoint = setpoint
	if changeID != uuid.Nil {
		command.Metadata["change_id"] = changeID.String()
	}
	if requestID != uuid.Nil {
		command.Metadata["request_id"] = requestID.String()
	}
	return command, nil
}

func (s *Service) recordEvent(ctx context.Context, lineID model.LineID, generation uint64, kind model.EventKind, value any, now time.Time) error {
	event, err := model.NewEvent(lineID, generation, kind, value, now)
	if err != nil {
		return err
	}
	_, err = s.journal.Append(ctx, event)
	return err
}

func (s *Service) deviceID(chemical string) string {
	return "doser-" + chemical
}

func (s *Service) GetRequest(id uuid.UUID) (Request, bool) {
	return s.requests.Get(id)
}

func (s *Service) PendingRequests(lineID model.LineID) []Request {
	if lineID == "" {
		return s.requests.Pending()
	}
	return s.requests.Pending(string(lineID))
}

func (s *Service) AllRequests() []Request {
	return s.requests.All()
}

func (s *Service) Latest(batchID uuid.UUID) (Change, bool) {
	return s.ledger.Latest(batchID)
}

func (s *Service) History(batchID uuid.UUID) []Change {
	return s.ledger.History(batchID)
}
