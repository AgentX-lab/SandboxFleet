package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/AgentNaut/SandboxFleet/internal/slot"
	"github.com/AgentNaut/SandboxFleet/internal/worker"
	"k8s.io/apimachinery/pkg/types"
)

type Manager interface {
	ReserveSlot(ctx context.Context, ref worker.SandboxSlotRef) error
	StartSandbox(ctx context.Context, req worker.StartSandboxRequest) error
	StopSandbox(ctx context.Context, ref worker.SandboxSlotRef) error
	ReleaseSlot(ctx context.Context, ref worker.SandboxSlotRef) error
	GetSandbox(ctx context.Context, ref worker.SandboxSlotRef) (worker.SandboxInfo, error)
	ExecSandbox(ctx context.Context, req worker.ExecSandboxRequest) (worker.ExecSandboxResult, error)
	ListSlots(ctx context.Context) []slot.Info
}

type Server struct {
	manager Manager
}

func NewServer(manager Manager) http.Handler {
	server := &Server{manager: manager}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", server.health)
	mux.HandleFunc("GET /v1/slots", server.listSlots)
	mux.HandleFunc("GET /v1/slots/{slotID}", server.getSandbox)
	mux.HandleFunc("POST /v1/slots/{slotID}/reserve", server.reserveSlot)
	mux.HandleFunc("POST /v1/slots/{slotID}/start", server.startSandbox)
	mux.HandleFunc("POST /v1/slots/{slotID}/stop", server.stopSandbox)
	mux.HandleFunc("POST /v1/slots/{slotID}/release", server.releaseSlot)
	mux.HandleFunc("POST /v1/slots/{slotID}/exec", server.execSandbox)
	return mux
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listSlots(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.manager.ListSlots(r.Context()))
}

func (s *Server) getSandbox(w http.ResponseWriter, r *http.Request) {
	slotID, ok := pathSlotID(w, r)
	if !ok {
		return
	}
	info, err := s.manager.GetSandbox(r.Context(), worker.SandboxSlotRef{
		SlotID: slotID,
		Identity: worker.SandboxIdentity{
			Namespace: r.URL.Query().Get("namespace"),
			Name:      r.URL.Query().Get("name"),
			UID:       types.UID(r.URL.Query().Get("uid")),
		},
	})
	if err != nil {
		writeManagerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) reserveSlot(w http.ResponseWriter, r *http.Request) {
	var ref worker.SandboxSlotRef
	if !decodeRequest(w, r, &ref) || !matchSlotID(w, r, ref.SlotID) {
		return
	}
	if err := s.manager.ReserveSlot(r.Context(), ref); err != nil {
		writeManagerError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) startSandbox(w http.ResponseWriter, r *http.Request) {
	var req worker.StartSandboxRequest
	if !decodeRequest(w, r, &req) || !matchSlotID(w, r, req.SlotID) {
		return
	}
	if err := s.manager.StartSandbox(r.Context(), req); err != nil {
		writeManagerError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) stopSandbox(w http.ResponseWriter, r *http.Request) {
	var ref worker.SandboxSlotRef
	if !decodeRequest(w, r, &ref) || !matchSlotID(w, r, ref.SlotID) {
		return
	}
	if err := s.manager.StopSandbox(r.Context(), ref); err != nil {
		writeManagerError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) releaseSlot(w http.ResponseWriter, r *http.Request) {
	var ref worker.SandboxSlotRef
	if !decodeRequest(w, r, &ref) || !matchSlotID(w, r, ref.SlotID) {
		return
	}
	if err := s.manager.ReleaseSlot(r.Context(), ref); err != nil {
		writeManagerError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) execSandbox(w http.ResponseWriter, r *http.Request) {
	var req worker.ExecSandboxRequest
	if !decodeRequest(w, r, &req) || !matchSlotID(w, r, req.SlotID) {
		return
	}
	result, err := s.manager.ExecSandbox(r.Context(), req)
	if err != nil {
		writeManagerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func pathSlotID(w http.ResponseWriter, r *http.Request) (int32, bool) {
	value, err := strconv.ParseInt(r.PathValue("slotID"), 10, 32)
	if err != nil || value < 0 {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "slotID must be a non-negative integer")
		return 0, false
	}
	return int32(value), true
}

func matchSlotID(w http.ResponseWriter, r *http.Request, requestSlotID int32) bool {
	pathID, ok := pathSlotID(w, r)
	if !ok {
		return false
	}
	if pathID != requestSlotID {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "path slotID does not match request slotID")
		return false
	}
	return true
}

func decodeRequest(w http.ResponseWriter, r *http.Request, target any) bool {
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "request body is not valid JSON")
		return false
	}
	return true
}

func writeManagerError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, worker.ErrInvalidRequest):
		writeError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
	case errors.Is(err, worker.ErrSlotNotFound), errors.Is(err, worker.ErrSandboxNotFound):
		writeError(w, http.StatusNotFound, "SandboxNotFound", err.Error())
	case errors.Is(err, worker.ErrSlotConflict):
		writeError(w, http.StatusConflict, "SlotConflict", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "RuntimeError", err.Error())
	}
}

type errorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorResponse{Code: code, Message: message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		http.Error(w, "encode response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}
