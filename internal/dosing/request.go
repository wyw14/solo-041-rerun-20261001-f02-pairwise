package dosing

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/model"
)

// RequestStatus 是高风险剂量变更申请单的状态。
type RequestStatus string

const (
	StatusPending  RequestStatus = "pending"  // 待审核，尚未进入设备总线
	StatusApproved RequestStatus = "approved" // 已批准并下发
	StatusRejected RequestStatus = "rejected" // 已拒绝，从未下发
	StatusRevoked  RequestStatus = "revoked"  // 已撤销；若曾下发则已生成安全补偿命令
)

// Request 是一次高风险剂量变更申请，完整保留旧值、新值、药剂与关联批次。
type Request struct {
	ID             uuid.UUID     `json:"id"`
	LineID         model.LineID  `json:"line_id"`
	BatchID        uuid.UUID     `json:"batch_id"`
	Chemical       string        `json:"chemical"`
	PreviousRate   float64       `json:"previous_rate"`
	Rate           float64       `json:"rate"`
	Risk           RiskLevel     `json:"risk"`
	RiskReasons    []string      `json:"risk_reasons"`
	Status         RequestStatus `json:"status"`
	Generation     uint64        `json:"generation"`
	RequestedBy    string        `json:"requested_by"`
	ReviewedBy     string        `json:"reviewed_by,omitempty"`
	ReviewNote     string        `json:"review_note,omitempty"`
	RequestedAt    time.Time     `json:"requested_at"`
	ReviewedAt     *time.Time    `json:"reviewed_at,omitempty"`
	CommandID      *uuid.UUID    `json:"command_id,omitempty"`
	RevokedAt      *time.Time    `json:"revoked_at,omitempty"`
	RevokedBy      string        `json:"revoked_by,omitempty"`
	CompensationID *uuid.UUID    `json:"compensation_id,omitempty"`
}

func newRequest(lineID model.LineID, batchID uuid.UUID, chemical string, previousRate, rate float64, generation uint64, requestedBy string, reasons []string, now time.Time) (Request, error) {
	if lineID == "" || batchID == uuid.Nil || chemical == "" || rate <= 0 || generation == 0 {
		return Request{}, errors.New("dose request is incomplete")
	}
	if requestedBy == "" {
		return Request{}, errors.New("dose request applicant is required")
	}
	return Request{
		ID: uuid.New(), LineID: lineID, BatchID: batchID, Chemical: chemical,
		PreviousRate: previousRate, Rate: rate, Risk: RiskHigh, RiskReasons: reasons,
		Status: StatusPending, Generation: generation, RequestedBy: requestedBy,
		RequestedAt: now.UTC(),
	}, nil
}

func (r Request) decide(status RequestStatus, reviewer, note string, now time.Time) (Request, error) {
	if r.Status != StatusPending {
		return r, errors.New("dose request is not awaiting review")
	}
	if reviewer == "" {
		return r, errors.New("dose review reviewer is required")
	}
	if reviewer == r.RequestedBy {
		return r, errors.New("dose change cannot be approved by the applicant")
	}
	if status != StatusApproved && status != StatusRejected {
		return r, errors.New("dose review decision is invalid")
	}
	stamp := now.UTC()
	r.Status = status
	r.ReviewedBy = reviewer
	r.ReviewNote = note
	r.ReviewedAt = &stamp
	return r, nil
}

func (r Request) revoke(operator string, now time.Time) (Request, error) {
	if operator == "" {
		return r, errors.New("dose revoke operator is required")
	}
	if r.Status != StatusApproved && r.Status != StatusPending {
		return r, errors.New("dose request can only be revoked while pending or approved")
	}
	stamp := now.UTC()
	r.Status = StatusRevoked
	r.RevokedBy = operator
	r.RevokedAt = &stamp
	return r, nil
}

// Issued 表示申请批准后命令是否已经进入设备总线。
func (r Request) Issued() bool {
	return r.Status == StatusApproved && r.CommandID != nil
}
