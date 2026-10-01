package dosing

import (
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/model"
)

// Change 是一次已经生效并下发到设备总线的加药速率变更。
type Change struct {
	ID           uuid.UUID    `json:"id"`
	LineID       model.LineID `json:"line_id"`
	BatchID      uuid.UUID    `json:"batch_id"`
	Chemical     string       `json:"chemical"`
	PreviousRate float64      `json:"previous_rate"`
	Rate         float64      `json:"rate"`
	Generation   uint64       `json:"generation"`
	// RequestID 非空时表示该变更由高风险申请批准产生；普通直发变更为空。
	RequestID   uuid.UUID `json:"request_id,omitempty"`
	Reason      string    `json:"reason,omitempty"`
	RequestedAt time.Time `json:"requested_at"`
	AppliedAt   time.Time `json:"applied_at"`
}

func NewChange(lineID model.LineID, batchID uuid.UUID, chemical string, previousRate, rate float64, generation uint64, now time.Time) (Change, error) {
	if lineID == "" || batchID == uuid.Nil || chemical == "" || rate < 0 || generation == 0 {
		return Change{}, errors.New("dosing change is incomplete")
	}
	if previousRate < 0 {
		return Change{}, errors.New("dosing previous rate cannot be negative")
	}
	return Change{
		ID: uuid.New(), LineID: lineID, BatchID: batchID, Chemical: chemical,
		PreviousRate: previousRate, Rate: rate, Generation: generation,
		RequestedAt: now.UTC(), AppliedAt: now.UTC(),
	}, nil
}

type ledgerKey struct {
	batch    uuid.UUID
	chemical string
}

// Ledger 仅记录已批准（或普通范围直接生效）并进入设备总线的变更，
// 未批准的申请不会出现在台账中。
type Ledger struct {
	mu      sync.RWMutex
	changes map[ledgerKey][]Change
}

func NewLedger() *Ledger {
	return &Ledger{changes: make(map[ledgerKey][]Change)}
}

func (l *Ledger) Record(change Change) {
	key := ledgerKey{change.BatchID, change.Chemical}
	l.mu.Lock()
	l.changes[key] = append(l.changes[key], change)
	l.mu.Unlock()
}

func (l *Ledger) Current(batchID uuid.UUID, chemical string) (Change, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	values := l.changes[ledgerKey{batchID, chemical}]
	if len(values) == 0 {
		return Change{}, false
	}
	return values[len(values)-1], true
}

// Latest 返回某批次最近一次生效的变更（任意药剂），兼容按批次聚合的状态视图。
func (l *Ledger) Latest(batchID uuid.UUID) (Change, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var latest Change
	found := false
	for key, values := range l.changes {
		if key.batch != batchID || len(values) == 0 {
			continue
		}
		candidate := values[len(values)-1]
		if !found || candidate.AppliedAt.After(latest.AppliedAt) {
			latest = candidate
			found = true
		}
	}
	return latest, found
}

// History 返回某批次全部已生效变更，按生效时间排序。
func (l *Ledger) History(batchID uuid.UUID) []Change {
	l.mu.RLock()
	defer l.mu.RUnlock()
	result := make([]Change, 0)
	for key, values := range l.changes {
		if key.batch == batchID {
			result = append(result, values...)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].AppliedAt.Before(result[j].AppliedAt) })
	return result
}
