package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

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
