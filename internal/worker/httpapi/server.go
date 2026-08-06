package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"

	"github.com/AgentNaut/SandboxFleet/internal/slot"
	"github.com/AgentNaut/SandboxFleet/internal/workerapi"
	"k8s.io/apimachinery/pkg/types"
)

type Manager interface {
	ReserveSlot(ctx context.Context, ref workerapi.SandboxSlotRef) error
	StartSandbox(ctx context.Context, req workerapi.StartSandboxRequest) error
	StopSandbox(ctx context.Context, ref workerapi.SandboxSlotRef) error
	ReleaseSlot(ctx context.Context, ref workerapi.SandboxSlotRef) error
	GetSandbox(ctx context.Context, ref workerapi.SandboxSlotRef) (workerapi.SandboxInfo, error)
	ExecSandbox(ctx context.Context, req workerapi.ExecSandboxRequest) (workerapi.ExecSandboxResult, error)
	CreateSnapshot(ctx context.Context, req workerapi.CreateSnapshotRequest) (workerapi.CreateSnapshotResult, error)
	RestoreFromSnapshot(ctx context.Context, req workerapi.RestoreFromSnapshotRequest) error
	DeleteSnapshotObjects(ctx context.Context, req workerapi.DeleteSnapshotObjectsRequest) error
	ExistsSandboxFile(ctx context.Context, req workerapi.SandboxFileRequest) (bool, error)
	ListSandboxFiles(ctx context.Context, req workerapi.SandboxFileRequest) ([]workerapi.SandboxFileEntry, error)
	ReadSandboxFile(ctx context.Context, req workerapi.SandboxFileRequest) ([]byte, error)
	WriteSandboxFile(ctx context.Context, req workerapi.SandboxFileRequest, content []byte) error
	ListSlots(ctx context.Context) []slot.Info
	ApplySlots(configs []slot.Config) error
}

type Server struct {
	manager Manager
}

func NewServer(manager Manager) http.Handler {
	server := &Server{manager: manager}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", server.health)
	mux.HandleFunc("PUT /v1/topology", server.putTopology)
	mux.HandleFunc("GET /v1/slots", server.listSlots)
	mux.HandleFunc("GET /v1/slots/{slotID}", server.getSandbox)
	mux.HandleFunc("POST /v1/slots/{slotID}/reserve", server.reserveSlot)
	mux.HandleFunc("POST /v1/slots/{slotID}/start", server.startSandbox)
	mux.HandleFunc("POST /v1/slots/{slotID}/stop", server.stopSandbox)
	mux.HandleFunc("POST /v1/slots/{slotID}/release", server.releaseSlot)
	mux.HandleFunc("POST /v1/slots/{slotID}/exec", server.execSandbox)
	mux.HandleFunc("POST /v1/slots/{slotID}/createSnapshot", server.createSnapshot)
	mux.HandleFunc("POST /v1/slots/{slotID}/restoreFromSnapshot", server.restoreFromSnapshot)
	mux.HandleFunc("POST /v1/snapshots/delete", server.deleteSnapshotObjects)
	mux.HandleFunc("GET /v1/slots/{slotID}/files/exists", server.fileExists)
	mux.HandleFunc("GET /v1/slots/{slotID}/files/list", server.listFiles)
	mux.HandleFunc("GET /v1/slots/{slotID}/files/content", server.readFile)
	mux.HandleFunc("POST /v1/slots/{slotID}/files/upload", server.writeFile)
	return mux
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) putTopology(w http.ResponseWriter, r *http.Request) {
	var configs []slot.Config
	if !decodeRequest(w, r, &configs) {
		return
	}
	if configs == nil {
		configs = []slot.Config{}
	}
	if err := s.manager.ApplySlots(configs); err != nil {
		writeManagerError(w, err)
		return
	}
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
	info, err := s.manager.GetSandbox(r.Context(), workerapi.SandboxSlotRef{
		SlotID: slotID,
		Identity: workerapi.SandboxIdentity{
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
	var ref workerapi.SandboxSlotRef
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
	var req workerapi.StartSandboxRequest
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
	var ref workerapi.SandboxSlotRef
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
	var ref workerapi.SandboxSlotRef
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
	var req workerapi.ExecSandboxRequest
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

func (s *Server) createSnapshot(w http.ResponseWriter, r *http.Request) {
	var req workerapi.CreateSnapshotRequest
	if !decodeRequest(w, r, &req) || !matchSlotID(w, r, req.SlotID) {
		return
	}
	result, err := s.manager.CreateSnapshot(r.Context(), req)
	if err != nil {
		writeManagerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) restoreFromSnapshot(w http.ResponseWriter, r *http.Request) {
	var req workerapi.RestoreFromSnapshotRequest
	if !decodeRequest(w, r, &req) || !matchSlotID(w, r, req.SlotID) {
		return
	}
	if err := s.manager.RestoreFromSnapshot(r.Context(), req); err != nil {
		writeManagerError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteSnapshotObjects(w http.ResponseWriter, r *http.Request) {
	var req workerapi.DeleteSnapshotObjectsRequest
	if !decodeRequest(w, r, &req) {
		return
	}
	if err := s.manager.DeleteSnapshotObjects(r.Context(), req); err != nil {
		writeManagerError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) fileExists(w http.ResponseWriter, r *http.Request) {
	req, ok := fileRequestFromQuery(w, r)
	if !ok {
		return
	}
	exists, err := s.manager.ExistsSandboxFile(r.Context(), req)
	if err != nil {
		writeManagerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"exists": exists})
}

func (s *Server) listFiles(w http.ResponseWriter, r *http.Request) {
	req, ok := fileRequestFromQuery(w, r)
	if !ok {
		return
	}
	entries, err := s.manager.ListSandboxFiles(r.Context(), req)
	if err != nil {
		writeManagerError(w, err)
		return
	}
	if entries == nil {
		entries = []workerapi.SandboxFileEntry{}
	}
	writeJSON(w, http.StatusOK, entries)
}

func (s *Server) readFile(w http.ResponseWriter, r *http.Request) {
	req, ok := fileRequestFromQuery(w, r)
	if !ok {
		return
	}
	data, err := s.manager.ReadSandboxFile(r.Context(), req)
	if err != nil {
		writeManagerError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) writeFile(w http.ResponseWriter, r *http.Request) {
	slotID, ok := pathSlotID(w, r)
	if !ok {
		return
	}
	identity := workerapi.SandboxIdentity{
		Namespace: r.URL.Query().Get("namespace"),
		Name:      r.URL.Query().Get("name"),
		UID:       types.UID(r.URL.Query().Get("uid")),
	}
	if err := r.ParseMultipartForm(workerapi.MaxFileBytes + (1 << 20)); err != nil {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "multipart form is required")
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "multipart field \"file\" is required")
		return
	}
	defer file.Close()
	content, err := readUpload(file, workerapi.MaxFileBytes)
	if err != nil {
		writeError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
		return
	}
	name := header.Filename
	if name == "" {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "upload filename is required")
		return
	}
	if err := s.manager.WriteSandboxFile(r.Context(), workerapi.SandboxFileRequest{
		SlotID:   slotID,
		Identity: identity,
		Path:     name,
	}, content); err != nil {
		writeManagerError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func fileRequestFromQuery(w http.ResponseWriter, r *http.Request) (workerapi.SandboxFileRequest, bool) {
	slotID, ok := pathSlotID(w, r)
	if !ok {
		return workerapi.SandboxFileRequest{}, false
	}
	pathValue := r.URL.Query().Get("path")
	if pathValue == "" {
		writeError(w, http.StatusBadRequest, "InvalidRequest", "path query parameter is required")
		return workerapi.SandboxFileRequest{}, false
	}
	return workerapi.SandboxFileRequest{
		SlotID: slotID,
		Identity: workerapi.SandboxIdentity{
			Namespace: r.URL.Query().Get("namespace"),
			Name:      r.URL.Query().Get("name"),
			UID:       types.UID(r.URL.Query().Get("uid")),
		},
		Path: pathValue,
	}, true
}

func readUpload(file multipart.File, limit int) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, errors.New("uploaded file exceeds size limit")
	}
	return data, nil
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
	case errors.Is(err, workerapi.ErrInvalidRequest), errors.Is(err, workerapi.ErrSlotConfigInvalid):
		writeError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
	case errors.Is(err, workerapi.ErrSnapshotLoad):
		// Permanent restore failure — do not 500-retry for 10 minutes.
		writeError(w, http.StatusUnprocessableEntity, "SnapshotLoadFailed", err.Error())
	case errors.Is(err, workerapi.ErrSlotNotFound), errors.Is(err, workerapi.ErrSandboxNotFound):
		writeError(w, http.StatusNotFound, "SandboxNotFound", err.Error())
	case errors.Is(err, workerapi.ErrSlotConflict):
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
