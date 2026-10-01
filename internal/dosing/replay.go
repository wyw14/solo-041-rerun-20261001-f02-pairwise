package dosing

import (
	"context"
	"errors"
	"fmt"

	"github.com/wyw14/cry-97/internal/model"
)

// ReplayEvent 在进程恢复时重建加药台账与申请簿，并把已下发命令幂等重放到设备总线。
// 未批准（request_submitted、request_reviewed 且驳回、request_canceled）的申请不会产生总线命令。
func (s *Service) ReplayEvent(ctx context.Context, event model.Event) error {
	switch event.Kind {
	case model.EventDosingChanged:
		var change Change
		if err := event.Decode(&change); err != nil {
			return err
		}
		if change.LineID != event.LineID {
			return errors.New("dosing change recovery payload does not match event identity")
		}
		if _, found := s.ledger.Find(change.BatchID, change.ID); found {
			return nil
		}
		s.ledger.Record(change)
		command, err := s.buildCommand(change, event.OccurredAt)
		if err != nil {
			return err
		}
		return s.safePublish(ctx, command)
	case model.EventDoseRequested:
		var request ChangeRequest
		if err := event.Decode(&request); err != nil {
			return err
		}
		return s.requests.Save(request)
	case model.EventDoseCanceled:
		var request ChangeRequest
		if err := event.Decode(&request); err != nil {
			return err
		}
		return s.requests.Save(request)
	case model.EventDoseReviewed:
		var record ReviewRecord
		if err := event.Decode(&record); err != nil {
			return err
		}
		if err := s.requests.Save(record.Request); err != nil {
			return err
		}
		if record.Dispatch == nil {
			return nil
		}
		change := record.Dispatch.Change
		if _, found := s.ledger.Find(change.BatchID, change.ID); !found {
			s.ledger.Record(change)
		}
		return s.safePublish(ctx, record.Dispatch.Command)
	case model.EventDoseRevoked:
		var record RevocationRecord
		if err := event.Decode(&record); err != nil {
			return err
		}
		if err := s.requests.Save(record.Request); err != nil {
			return err
		}
		if _, found := s.ledger.Find(record.CompensationChange.BatchID, record.CompensationChange.ID); !found {
			s.ledger.Record(record.CompensationChange)
		}
		// 直接重放落盘的补偿命令，保留撤销时确定的 stop/set 动作与目标值。
		return s.safePublish(ctx, record.Compensation)
	default:
		return nil
	}
}

// safePublish 重放已落盘命令；命令代际已被急停等流程推进时跳过，不阻断其余事件重放。
func (s *Service) safePublish(ctx context.Context, command model.DeviceCommand) error {
	if err := s.publisher.Publish(ctx, command); err != nil {
		if errors.Is(err, model.ErrCommandStale) {
			return nil
		}
		return fmt.Errorf("replay dosing command %s: %w", command.ID, err)
	}
	return nil
}
