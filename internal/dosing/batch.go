package dosing

import (
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/model"
)

// Change 是一次已经进入设备总线的加药设定值变更。
type Change struct {
	ID           uuid.UUID    `json:"id"`
	RequestID    uuid.UUID    `json:"request_id,omitempty"`
	LineID       model.LineID `json:"line_id"`
	BatchID      uuid.UUID    `json:"batch_id"`
	Chemical     string       `json:"chemical"`
	OldRate      float64      `json:"old_rate"`
	Rate         float64      `json:"rate"`
	HighRisk     bool         `json:"high_risk"`
	Compensation bool         `json:"compensation,omitempty"`
	Generation   uint64       `json:"generation"`
	RequestedAt  time.Time    `json:"requested_at"`
}

func NewChange(lineID model.LineID, batchID uuid.UUID, chemical string, oldRate, rate float64, generation uint64, highRisk bool, requestID uuid.UUID, now time.Time) (Change, error) {
	if lineID == "" || batchID == uuid.Nil || chemical == "" || rate <= 0 || generation == 0 {
		return Change{}, errors.New("dosing change is incomplete")
	}
	if oldRate < 0 {
		return Change{}, errors.New("dosing old rate cannot be negative")
	}
	return Change{
		ID: uuid.New(), RequestID: requestID, LineID: lineID, BatchID: batchID, Chemical: chemical,
		OldRate: oldRate, Rate: rate, HighRisk: highRisk, Generation: generation, RequestedAt: now.UTC(),
	}, nil
}

// NewCompensationChange 构造撤销后的安全补偿记录：恢复撤销前的旧值，旧值为 0 时表示停机。
func NewCompensationChange(revoked Change, generation uint64, now time.Time) Change {
	return Change{
		ID: uuid.New(), RequestID: revoked.RequestID, LineID: revoked.LineID, BatchID: revoked.BatchID,
		Chemical: revoked.Chemical, OldRate: revoked.Rate, Rate: revoked.OldRate, HighRisk: false,
		Compensation: true, Generation: generation, RequestedAt: now.UTC(),
	}
}

// Ledger 保留批次下全部已下发变更，最新值在末尾，历史不删除。
type Ledger struct {
	mu      sync.RWMutex
	changes map[uuid.UUID][]Change
}

func NewLedger() *Ledger {
	return &Ledger{changes: make(map[uuid.UUID][]Change)}
}

func (l *Ledger) Record(change Change) {
	l.mu.Lock()
	l.changes[change.BatchID] = append(l.changes[change.BatchID], change)
	l.mu.Unlock()
}

func (l *Ledger) Latest(batchID uuid.UUID) (Change, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	values := l.changes[batchID]
	if len(values) == 0 {
		return Change{}, false
	}
	return values[len(values)-1], true
}

// LatestForChemical 返回指定批次、指定药剂最近一次下发的设定值。
func (l *Ledger) LatestForChemical(batchID uuid.UUID, chemical string) (Change, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for index := len(l.changes[batchID]) - 1; index >= 0; index-- {
		if value := l.changes[batchID][index]; value.Chemical == chemical {
			return value, true
		}
	}
	return Change{}, false
}

// Find 返回指定批次中的某次已下发变更。
func (l *Ledger) Find(batchID, changeID uuid.UUID) (Change, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, value := range l.changes[batchID] {
		if value.ID == changeID {
			return value, true
		}
	}
	return Change{}, false
}

// History 返回批次下全部已下发变更（含安全补偿），按下发顺序排列。
func (l *Ledger) History(batchID uuid.UUID) []Change {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return append([]Change(nil), l.changes[batchID]...)
}

// Current 返回批次下每种药剂最近一次下发的设定值（补偿命令之后即为补偿值）。
func (l *Ledger) Current(batchID uuid.UUID) []Change {
	l.mu.RLock()
	defer l.mu.RUnlock()
	latest := make(map[string]Change)
	order := make([]string, 0)
	for _, value := range l.changes[batchID] {
		if _, seen := latest[value.Chemical]; !seen {
			order = append(order, value.Chemical)
		}
		latest[value.Chemical] = value
	}
	result := make([]Change, 0, len(order))
	for _, chemical := range order {
		result = append(result, latest[chemical])
	}
	return result
}

// RateBefore 返回 changeID 之前同药剂的设定值；此前从未投加时返回 0。
func (l *Ledger) RateBefore(batchID uuid.UUID, chemical string, changeID uuid.UUID) float64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var previous float64
	for _, value := range l.changes[batchID] {
		if value.ID == changeID {
			break
		}
		if value.Chemical == chemical {
			previous = value.Rate
		}
	}
	return previous
}
