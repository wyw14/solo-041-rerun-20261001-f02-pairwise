package dosing

import (
	"errors"
	"sort"
	"sync"

	"github.com/google/uuid"
)

// RequestBook 保存高风险剂量变更申请单的当前状态与历史。
type RequestBook struct {
	mu       sync.RWMutex
	requests map[uuid.UUID]Request
}

func NewRequestBook() *RequestBook {
	return &RequestBook{requests: make(map[uuid.UUID]Request)}
}

func (b *RequestBook) Put(request Request) {
	b.mu.Lock()
	b.requests[request.ID] = request
	b.mu.Unlock()
}

func (b *RequestBook) Get(id uuid.UUID) (Request, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	request, ok := b.requests[id]
	return request, ok
}

// Pending 返回全部待审核申请（可按处理线过滤），按申请时间排序。
func (b *RequestBook) Pending(lineID ...string) []Request {
	filter := ""
	if len(lineID) > 0 {
		filter = lineID[0]
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	result := make([]Request, 0)
	for _, request := range b.requests {
		if request.Status != StatusPending {
			continue
		}
		if filter != "" && string(request.LineID) != filter {
			continue
		}
		result = append(result, request)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].RequestedAt.Before(result[j].RequestedAt) })
	return result
}

// All 返回全部申请（含已终结历史），按申请时间排序。
func (b *RequestBook) All() []Request {
	b.mu.RLock()
	defer b.mu.RUnlock()
	result := make([]Request, 0, len(b.requests))
	for _, request := range b.requests {
		result = append(result, request)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].RequestedAt.Before(result[j].RequestedAt) })
	return result
}

// MustGet 与 Get 相同，但未找到时返回错误。
func (b *RequestBook) MustGet(id uuid.UUID) (Request, error) {
	request, ok := b.Get(id)
	if !ok {
		return Request{}, errors.New("dose change request is not found")
	}
	return request, nil
}
