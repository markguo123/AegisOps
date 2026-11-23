package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/k8s-ai/k8s-ai/internal/service"
	"github.com/k8s-ai/k8s-ai/internal/store"
)

func (s *Server) WithDiagnosticMemories(memories *service.DiagnosticMemories) *Server {
	s.memories = memories
	return s
}

func (s *Server) handleDiagnosticMemories(w http.ResponseWriter, r *http.Request) {
	user := UserFrom(r.Context())
	if user == nil {
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "authentication required")
		return
	}
	if s.memories == nil {
		writeError(w, r, http.StatusNotImplemented, codeInternal, "diagnostic memory unavailable")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	var result any
	var err error
	status := http.StatusOK
	switch r.Method {
	case http.MethodGet:
		result, err = s.memories.List(r.Context(), user.ID)
	case http.MethodPost, http.MethodPatch:
		var input service.DiagnosticMemory
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&input) != nil {
			writeError(w, r, http.StatusBadRequest, codeInvalidArgument, "invalid diagnostic memory")
			return
		}
		if r.Method == http.MethodPatch {
			result, err = s.memories.Update(r.Context(), user.ID, r.PathValue("id"), input)
			break
		}
		result, err = s.memories.Save(r.Context(), user.ID, input)
		status = http.StatusCreated
	case http.MethodDelete:
		err = s.memories.Forget(r.Context(), user.ID, r.PathValue("id"))
	}
	switch {
	case errors.Is(err, service.ErrInvalidMemory):
		writeError(w, r, http.StatusBadRequest, codeInvalidArgument, "invalid diagnostic memory")
	case errors.Is(err, service.ErrMemoryDisabled):
		writeError(w, r, http.StatusForbidden, codeForbidden, "diagnostic memory is disabled")
	case errors.Is(err, store.ErrMemoryConflict):
		writeError(w, r, http.StatusConflict, "CONFLICT", "diagnostic memory has changed")
	case errors.Is(err, store.ErrNotFound):
		writeError(w, r, http.StatusNotFound, codeNotFound, "diagnostic memory or scope unavailable")
	case err != nil:
		writeError(w, r, http.StatusInternalServerError, codeInternal, "diagnostic memory request failed")
	default:
		s.recordDiagnosticMemoryChange(r, user.ID, result)
		writeOK(w, r, status, result)
	}
}

func (s *Server) recordDiagnosticMemoryChange(r *http.Request, owner string, result any) {
	if s.auditlog == nil || r.Method == http.MethodGet {
		return
	}
	action := "agent.memory.forget"
	id := r.PathValue("id")
	if memory, ok := result.(service.DiagnosticMemory); ok {
		id = memory.ID
		if r.Method == http.MethodPost {
			action = "agent.memory.save"
		} else {
			action = "agent.memory.update"
		}
	}
	err := s.auditlog.Record(r.Context(), store.AuditEvent{
		RequestID: RequestIDFrom(r.Context()), UserID: owner,
		Action: action, ResourceKind: "DiagnosticMemory", ResourceName: id, Status: "success",
	})
	if err != nil {
		slog.Warn("diagnostic memory audit write failed", "action", action, "memory_id", id)
	}
}
