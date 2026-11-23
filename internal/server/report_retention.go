package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/k8s-ai/k8s-ai/internal/service"
	"github.com/k8s-ai/k8s-ai/internal/store"
)

type reportRetentionPreviewer interface {
	PreviewReportRetention(context.Context, string, string, int, int) (store.ReportRetentionPreview, error)
}

func (s *Server) handleReportRetentionPreview(w http.ResponseWriter, r *http.Request) {
	scope, _, err := s.reportScopeFor(r.Context())
	if err != nil {
		writeReportScopeError(w, r, err)
		return
	}
	if !scope.Admin {
		writeError(w, r, http.StatusForbidden, codeForbidden, "administrator required")
		return
	}
	previewer, ok := s.reportSvc.(reportRetentionPreviewer)
	if !ok {
		writeError(w, r, http.StatusNotImplemented, codeInternal, "report retention preview is unavailable")
		return
	}
	query := r.URL.Query()
	clusterID := strings.TrimSpace(query.Get("clusterId"))
	kind := query.Get("kind")
	days, daysErr := strconv.Atoi(query.Get("days"))
	minimum, minimumErr := strconv.Atoi(query.Get("minimum"))
	if clusterID == "" || (kind != service.ReportKindLatestMD && kind != service.ReportKindLatestJSON) ||
		daysErr != nil || days < 1 || days > 36500 || minimumErr != nil || minimum < 1 {
		writeError(w, r, http.StatusBadRequest, codeInvalidArgument, "invalid report retention policy")
		return
	}
	result, err := previewer.PreviewReportRetention(r.Context(), clusterID, kind, days, minimum)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, r, http.StatusNotFound, codeNotFound, "cluster not found")
	case err != nil:
		writeError(w, r, http.StatusInternalServerError, codeInternal, "report retention preview failed")
	default:
		writeOK(w, r, http.StatusOK, result)
	}
}

type reportRetentionPolicies interface {
	ReportRetentionPolicies(context.Context) ([]store.ReportRetentionPolicy, error)
	SetReportRetentionPolicy(context.Context, store.ReportRetentionPolicy, time.Time, int64, string) error
}

func (s *Server) handleReportRetentionPolicies(w http.ResponseWriter, r *http.Request) {
	scope, user, err := s.reportScopeFor(r.Context())
	if err != nil {
		writeReportScopeError(w, r, err)
		return
	}
	if !scope.Admin {
		writeError(w, r, http.StatusForbidden, codeForbidden, "administrator required")
		return
	}
	policies, ok := s.reportSvc.(reportRetentionPolicies)
	if !ok {
		writeError(w, r, http.StatusNotImplemented, codeInternal, "report retention policies unavailable")
		return
	}
	if r.Method == http.MethodGet {
		items, err := policies.ReportRetentionPolicies(r.Context())
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, codeInternal, "report retention query failed")
			return
		}
		writeOK(w, r, http.StatusOK, items)
		return
	}
	var input struct {
		store.ReportRetentionPolicy
		Cutoff   time.Time `json:"cutoff"`
		Eligible int64     `json:"eligible"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&input); err != nil ||
		strings.TrimSpace(input.ClusterID) == "" || len(input.ClusterID) > 191 ||
		(input.Kind != service.ReportKindLatestMD && input.Kind != service.ReportKindLatestJSON) ||
		input.Days < 1 || input.Days > 36500 || input.Minimum < 1 || input.Minimum > 1_000_000 {
		writeError(w, r, http.StatusBadRequest, codeInvalidArgument, "invalid report retention policy")
		return
	}
	err = policies.SetReportRetentionPolicy(r.Context(), input.ReportRetentionPolicy, input.Cutoff, input.Eligible, user.ID)
	switch {
	case errors.Is(err, service.ErrRetentionPreviewRequired):
		writeError(w, r, http.StatusConflict, "CONFLICT", "report retention preview has expired or changed")
	case errors.Is(err, store.ErrNotFound):
		writeError(w, r, http.StatusNotFound, codeNotFound, "cluster not found")
	case err != nil:
		writeError(w, r, http.StatusInternalServerError, codeInternal, "report retention policy update failed")
	default:
		writeOK(w, r, http.StatusOK, nil)
	}
}
