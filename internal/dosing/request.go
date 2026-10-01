package dosing

import (
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/model"
)

type RequestStatus string

const (
	StatusPending  RequestStatus = "pending_approval"
	StatusApproved RequestStatus = "approved"
	StatusRejected RequestStatus = "rejected"
	StatusCanceled RequestStatus = "canceled"
	StatusRevoked  RequestStatus = "revoked"
)

type ReviewDecision string

const (
	DecisionApprove ReviewDecision = "approve"
	DecisionReject  ReviewDecision = "reject"
)

// ChangeRequest 是一次加药剂量变更申请。未批准前不产生设备总线命令。
type ChangeRequest struct {
	ID             uuid.UUID     `json:"id"`
	LineID         model.LineID  `json:"line_id"`
	BatchID        uuid.UUID     `json:"batch_id"`
	Chemical       string        `json:"chemical"`
	OldRate        float64       `json:"old_rate"`
	NewRate        float64       `json:"new_rate"`
	HighRisk       bool          `json:"high_risk"`
	Status         RequestStatus `json:"status"`
	Generation     uint64        `json:"generation"`
	Reason         string        `json:"reason,omitempty"`
	RequestedBy    string        `json:"requested_by"`
	RequestedAt    time.Time     `json:"requested_at"`
	ReviewedBy     string        `json:"reviewed_by,omitempty"`
	ReviewNote     string        `json:"review_note,omitempty"`
	ReviewedAt     *time.Time    `json:"reviewed_at,omitempty"`
	ChangeID       uuid.UUID     `json:"change_id,omitempty"`
	RevokedBy      string        `json:"revoked_by,omitempty"`
	RevokeNote     string        `json:"revoke_note,omitempty"`
	RevokedAt      *time.Time    `json:"revoked_at,omitempty"`
	CompensationID uuid.UUID     `json:"compensation_id,omitempty"`
}

func NewChangeRequest(lineID model.LineID, batchID uuid.UUID, chemical string, oldRate, newRate float64, generation uint64, highRisk bool, reason, requestedBy string, now time.Time) (ChangeRequest, error) {
	if lineID == "" || batchID == uuid.Nil || chemical == "" || newRate <= 0 || generation == 0 {
		return ChangeRequest{}, errors.New("dosing request is incomplete")
	}
	if oldRate < 0 {
		return ChangeRequest{}, errors.New("dosing old rate cannot be negative")
	}
	return ChangeRequest{
		ID: uuid.New(), LineID: lineID, BatchID: batchID, Chemical: chemical,
		OldRate: oldRate, NewRate: newRate, HighRisk: highRisk, Status: StatusPending,
		Generation: generation, Reason: reason, RequestedBy: requestedBy, RequestedAt: now.UTC(),
	}, nil
}

// Decide 记录审核结论。仅待审核申请可以被审核。
func (r ChangeRequest) Decide(decision ReviewDecision, reviewer, note string, now time.Time) (ChangeRequest, error) {
	if r.Status != StatusPending {
		return r, errors.New("dosing request is not awaiting review")
	}
	if reviewer == "" {
		return r, errors.New("reviewer is required")
	}
	switch decision {
	case DecisionApprove:
		r.Status = StatusApproved
	case DecisionReject:
		r.Status = StatusRejected
	default:
		return r, errors.New("review decision must be approve or reject")
	}
	r.ReviewedBy = reviewer
	r.ReviewNote = note
	stamp := now.UTC()
	r.ReviewedAt = &stamp
	return r, nil
}

// Cancel 由申请人在审核前撤回申请。
func (r ChangeRequest) Cancel(now time.Time) (ChangeRequest, error) {
	if r.Status != StatusPending {
		return r, errors.New("only pending dosing request can be canceled")
	}
	r.Status = StatusCanceled
	stamp := now.UTC()
	r.ReviewedAt = &stamp
	return r, nil
}

// MarkDispatched 在批准后命令下发成功时关联实际变更并标记历史。
func (r ChangeRequest) MarkDispatched(changeID uuid.UUID) (ChangeRequest, error) {
	if r.Status != StatusApproved || changeID == uuid.Nil {
		return r, errors.New("approved request cannot be linked to change")
	}
	r.ChangeID = changeID
	return r, nil
}

// Revoke 仅允许对已经下发的批准申请执行。
func (r ChangeRequest) Revoke(operator, note string, compensationID uuid.UUID, now time.Time) (ChangeRequest, error) {
	if r.Status != StatusApproved || r.ChangeID == uuid.Nil {
		return r, errors.New("only dispatched approved change can be revoked")
	}
	if operator == "" {
		return r, errors.New("revoking operator is required")
	}
	r.Status = StatusRevoked
	r.RevokedBy = operator
	r.RevokeNote = note
	r.CompensationID = compensationID
	stamp := now.UTC()
	r.RevokedAt = &stamp
	return r, nil
}

// RequestBook 保存全部申请（含终态），按提交顺序排列，历史不删除。
type RequestBook struct {
	mu       sync.RWMutex
	requests []ChangeRequest
	index    map[uuid.UUID]int
}

func NewRequestBook() *RequestBook {
	return &RequestBook{index: make(map[uuid.UUID]int)}
}

// Save 新建或更新申请。更新只允许终态向后流转，防止旧事件覆盖新状态。
func (b *RequestBook) Save(request ChangeRequest) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if position, ok := b.index[request.ID]; ok {
		current := b.requests[position]
		if !statusTransitionAllowed(current.Status, request.Status) {
			return errors.New("dosing request status regression is rejected")
		}
		b.requests[position] = request
		return nil
	}
	b.index[request.ID] = len(b.requests)
	b.requests = append(b.requests, request)
	return nil
}

func (b *RequestBook) Get(id uuid.UUID) (ChangeRequest, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	position, ok := b.index[id]
	if !ok {
		return ChangeRequest{}, false
	}
	return b.requests[position], true
}

// PendingFor 返回指定批次+药剂仍在待审核的申请，避免同一剂量点多份申请并发进总线。
func (b *RequestBook) PendingFor(batchID uuid.UUID, chemical string) (ChangeRequest, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for index := len(b.requests) - 1; index >= 0; index-- {
		request := b.requests[index]
		if request.BatchID == batchID && request.Chemical == chemical && request.Status == StatusPending {
			return request, true
		}
	}
	return ChangeRequest{}, false
}

func (b *RequestBook) List(filter RequestStatus) []ChangeRequest {
	b.mu.RLock()
	defer b.mu.RUnlock()
	result := make([]ChangeRequest, 0, len(b.requests))
	for _, request := range b.requests {
		if filter == "" || request.Status == filter {
			result = append(result, request)
		}
	}
	return result
}

var requestStatusOrder = map[RequestStatus]int{
	StatusPending: 1, StatusApproved: 2, StatusRejected: 2, StatusCanceled: 2, StatusRevoked: 3,
}

func statusTransitionAllowed(from, to RequestStatus) bool {
	if from == to {
		return true
	}
	return requestStatusOrder[to] > requestStatusOrder[from]
}
