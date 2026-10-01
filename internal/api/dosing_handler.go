package api

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/dosing"
	"github.com/wyw14/cry-97/internal/model"
	"github.com/wyw14/cry-97/internal/process"
)

type doseSubmitRequest struct {
	LineID      string  `json:"line_id"`
	BatchID     string  `json:"batch_id"`
	Chemical    string  `json:"chemical"`
	Rate        float64 `json:"rate"`
	Reason      string  `json:"reason"`
	RequestedBy string  `json:"requested_by"`
}

type doseReviewRequest struct {
	Decision   string `json:"decision"`
	Reviewer   string `json:"reviewer"`
	ReviewNote string `json:"review_note"`
}

type doseRevokeRequest struct {
	Operator   string `json:"operator"`
	RevokeNote string `json:"revoke_note"`
}

func (s *Server) submitDose(w http.ResponseWriter, r *http.Request) {
	var input doseSubmitRequest
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
		input.RequestedBy = "operator"
	}
	submission, err := s.plant.SubmitDoseChange(r.Context(), lineID, batchID, input.Chemical, input.Rate, input.Reason, input.RequestedBy)
	if err != nil {
		// 高风险但存在待审核申请等业务冲突返回 409；总线不可用返回 503。
		var publishErr *dosing.PublishError
		if errors.As(err, &publishErr) {
			writeError(w, http.StatusServiceUnavailable, err)
		} else {
			writeError(w, http.StatusConflict, err)
		}
		return
	}
	status := http.StatusAccepted
	if !submission.HighRisk {
		status = http.StatusOK
	}
	writeJSON(w, status, submission)
}

func (s *Server) listDoseRequests(w http.ResponseWriter, r *http.Request) {
	filter := dosing.RequestStatus(r.URL.Query().Get("status"))
	writeJSON(w, http.StatusOK, map[string]any{"requests": s.plant.DoseRequests(filter)})
}

func (s *Server) getDoseRequest(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "requestID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	request, ok := s.plant.DoseRequest(id)
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("dosing request is not found"))
		return
	}
	writeJSON(w, http.StatusOK, request)
}

func (s *Server) reviewDoseRequest(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "requestID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var input doseReviewRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	decision := dosing.ReviewDecision(input.Decision)
	if decision != dosing.DecisionApprove && decision != dosing.DecisionReject {
		writeError(w, http.StatusBadRequest, errors.New("decision must be approve or reject"))
		return
	}
	record, err := s.plant.ReviewDoseChange(r.Context(), id, decision, input.Reviewer, input.ReviewNote)
	if err != nil {
		var publishErr *dosing.PublishError
		switch {
		case errors.As(err, &publishErr):
			writeError(w, http.StatusServiceUnavailable, err)
		case errors.Is(err, process.ErrDoseRequestNotFound):
			writeError(w, http.StatusNotFound, err)
		default:
			writeError(w, http.StatusConflict, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, record)
}

func (s *Server) cancelDoseRequest(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "requestID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	request, err := s.plant.CancelDoseChange(r.Context(), id)
	if err != nil {
		if errors.Is(err, process.ErrDoseRequestNotFound) {
			writeError(w, http.StatusNotFound, err)
		} else {
			writeError(w, http.StatusConflict, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, request)
}

func (s *Server) revokeDoseRequest(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "requestID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var input doseRevokeRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if input.Operator == "" {
		input.Operator = "supervisor"
	}
	record, err := s.plant.RevokeDoseChange(r.Context(), id, input.Operator, input.RevokeNote)
	if err != nil {
		var publishErr *dosing.PublishError
		switch {
		case errors.As(err, &publishErr):
			writeError(w, http.StatusServiceUnavailable, err)
		case errors.Is(err, process.ErrDoseRequestNotFound):
			writeError(w, http.StatusNotFound, err)
		default:
			writeError(w, http.StatusConflict, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, record)
}
