package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/dosing"
	"github.com/wyw14/cry-97/internal/model"
)

// DoseController 由工艺总控 Plant 实现，封装普通直发与高风险变更的申请/审核/撤销。
type DoseController interface {
	ChangeDose(ctx context.Context, lineID model.LineID, batchID uuid.UUID, chemical string, rate float64, requestedBy string) (dosing.Outcome, error)
	ApproveDoseChange(ctx context.Context, requestID uuid.UUID, reviewer, note string) (dosing.Request, dosing.Change, model.DeviceCommand, error)
	RejectDoseChange(ctx context.Context, requestID uuid.UUID, reviewer, note string) (dosing.Request, error)
	RevokeDoseChange(ctx context.Context, requestID uuid.UUID, operator string) (dosing.Request, *model.DeviceCommand, error)
	PendingDoseRequests() []dosing.Request
	DoseRequests() []dosing.Request
}

type DoseHandler struct {
	controller DoseController
}

func NewDoseHandler(controller DoseController) *DoseHandler {
	return &DoseHandler{controller: controller}
}

type doseRequest struct {
	LineID      string  `json:"line_id"`
	BatchID     string  `json:"batch_id"`
	Chemical    string  `json:"chemical"`
	Rate        float64 `json:"rate"`
	RequestedBy string  `json:"requested_by"`
}

type doseReviewRequest struct {
	Reviewer string `json:"reviewer"`
	Operator string `json:"operator"`
	Note     string `json:"note"`
}

// Create 提交加药变更：普通范围返回 202 与已下发命令；高风险返回 202 与待审申请，
// 申请在批准前不会产生任何设备总线命令。
func (h *DoseHandler) Create(w http.ResponseWriter, r *http.Request) {
	if h.controller == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("dosing controller is unavailable"))
		return
	}
	var input doseRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	lineID, err := model.ParseLineID(input.LineID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	batchID, err := uuid.Parse(input.BatchID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if input.RequestedBy == "" {
		writeError(w, http.StatusBadRequest, errors.New("requested_by is required"))
		return
	}
	outcome, err := h.controller.ChangeDose(r.Context(), lineID, batchID, input.Chemical, input.Rate, input.RequestedBy)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusAccepted, outcome)
}

// List 返回全部高风险申请（含已终结历史）；?status=pending 仅返回待审核。
func (h *DoseHandler) List(w http.ResponseWriter, r *http.Request) {
	if h.controller == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("dosing controller is unavailable"))
		return
	}
	if r.URL.Query().Get("status") == "pending" {
		writeJSON(w, http.StatusOK, map[string]any{"requests": h.controller.PendingDoseRequests()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": h.controller.DoseRequests()})
}

func (h *DoseHandler) Approve(w http.ResponseWriter, r *http.Request) {
	h.review(w, r, true)
}

func (h *DoseHandler) Reject(w http.ResponseWriter, r *http.Request) {
	h.review(w, r, false)
}

func (h *DoseHandler) review(w http.ResponseWriter, r *http.Request, approve bool) {
	if h.controller == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("dosing controller is unavailable"))
		return
	}
	requestID, err := uuid.Parse(chi.URLParam(r, "requestID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var input doseReviewRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	reviewer := input.Reviewer
	if reviewer == "" {
		reviewer = input.Operator
	}
	if reviewer == "" {
		writeError(w, http.StatusBadRequest, errors.New("reviewer is required"))
		return
	}
	if approve {
		request, change, command, err := h.controller.ApproveDoseChange(r.Context(), requestID, reviewer, input.Note)
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"request": request, "change": change, "command": command})
		return
	}
	request, err := h.controller.RejectDoseChange(r.Context(), requestID, reviewer, input.Note)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"request": request})
}

func (h *DoseHandler) Revoke(w http.ResponseWriter, r *http.Request) {
	if h.controller == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("dosing controller is unavailable"))
		return
	}
	requestID, err := uuid.Parse(chi.URLParam(r, "requestID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var input doseReviewRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	operator := input.Operator
	if operator == "" {
		operator = input.Reviewer
	}
	if operator == "" {
		writeError(w, http.StatusBadRequest, errors.New("operator is required"))
		return
	}
	request, command, err := h.controller.RevokeDoseChange(r.Context(), requestID, operator)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"request": request, "command": command})
}
