package cli

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/k8s-ai/k8s-ai/internal/config"
	"github.com/k8s-ai/k8s-ai/internal/model"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/util/validation"
)

type inspectionRequest struct {
	Scope     string `json:"scope" jsonschema:"cluster, namespace or pod"`
	Namespace string `json:"namespace,omitempty"`
	Pod       string `json:"pod,omitempty"`
	Preset    string `json:"preset,omitempty" jsonschema:"quick, standard or deep"`
}
type inspectionID struct {
	ScanID string `json:"scanId"`
}
type inspectionStatus struct {
	ScanID     string `json:"scanId"`
	State      string `json:"state"`
	ReasonCode string `json:"reasonCode,omitempty"`
}
type inspectionReportRequest struct {
	inspectionID
	Offset int `json:"offset,omitempty"`
}
type inspectionReport struct {
	Scope      model.ScanMeta           `json:"observedScope"`
	Summary    model.ClusterSummary     `json:"summary"`
	Score      model.HealthScore        `json:"healthScore"`
	Collection *model.CollectionQuality `json:"collection,omitempty"`
	Findings   []model.Finding          `json:"findings,omitempty"`
	NextOffset *int                     `json:"nextOffset,omitempty"`
}
type inspectionJob struct {
	status  inspectionStatus
	result  *model.ScanResult
	expires time.Time
}

func newMCPCmd(deps Dependencies) *cobra.Command {
	root := &cobra.Command{Use: "mcp", Short: "Manage the standalone MCP server"}
	var listen string
	serve := &cobra.Command{Use: "serve", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		token := os.Getenv("AEGISOPS_MCP_TOKEN")
		if token == "" {
			return errors.New("AEGISOPS_MCP_TOKEN is required")
		}
		load := loadOptionsFromFlags(cmd, true)
		load.Overrides["platform.store.driver"] = config.StoreDriverSQLite
		cfg, err := deps.LoadConfig(load)
		if err != nil {
			return err
		}
		opts := scanOptionsFromConfig(cfg)
		opts.ReportMode = "none"
		opts.LLM.Enabled = false
		host, _, err := net.SplitHostPort(listen)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return errors.New("use a loopback listen address and an HTTPS reverse proxy for remote clients")
		}
		ctx, cancel := context.WithCancel(cmd.Context())
		defer cancel()
		runner := deps.NewScan()
		handler := inspectionHandler(ctx, opts, runner.Run)
		mux := http.NewServeMux()
		mux.Handle("/mcp", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, "authentication required", http.StatusUnauthorized)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
			handler.ServeHTTP(w, r)
		}))
		server := &http.Server{Addr: listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second}
		stop := context.AfterFunc(ctx, func() { _ = server.Close() })
		defer stop()
		err = server.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}}
	serve.Flags().StringVar(&listen, "listen", "127.0.0.1:8090", "Loopback HTTP listen address")
	serve.Flags().String("kubeconfig", "", "Kubernetes kubeconfig")
	serve.Flags().String("context", "", "Kubernetes context")
	root.AddCommand(serve)
	return root
}

func inspectionHandler(ctx context.Context, base model.ScanOptions, scan func(context.Context, model.ScanOptions) (*model.ScanResult, error)) http.Handler {
	var mu sync.Mutex
	jobs := make(map[string]*inspectionJob)
	running := false
	server := sdk.NewServer(&sdk.Implementation{Name: "AegisOps inspection"}, nil)
	sdk.AddTool(server, &sdk.Tool{Name: "start_inspection", Description: "Start an asynchronous Kubernetes inspection"}, func(_ context.Context, _ *sdk.CallToolRequest, in inspectionRequest) (*sdk.CallToolResult, inspectionStatus, error) {
		opts := base
		if !validInspection(in) || base.Namespace != "" && in.Namespace != base.Namespace {
			return nil, inspectionStatus{}, errors.New("invalid inspection scope")
		}
		opts.Namespace, opts.PodTarget = in.Namespace, in.Pod
		opts.CollectEvents = true
		opts.CollectLogs, opts.CollectPreviousLogs = false, false
		switch in.Preset {
		case "quick":
			opts.Timeout = min(opts.Timeout, time.Minute)
		case "", "standard":
			opts.Timeout = min(opts.Timeout, 5*time.Minute)
		case "deep":
			opts.Timeout = min(opts.Timeout, 10*time.Minute)
			opts.CollectLogs, opts.CollectPreviousLogs = base.CollectLogs, base.CollectPreviousLogs
			opts.Since = time.Hour
		default:
			return nil, inspectionStatus{}, errors.New("invalid inspection preset")
		}
		mu.Lock()
		defer mu.Unlock()
		for id, job := range jobs {
			if !job.expires.IsZero() && time.Now().After(job.expires) {
				delete(jobs, id)
			}
		}
		if running || len(jobs) >= 16 {
			return nil, inspectionStatus{}, errors.New("inspection service at capacity")
		}
		status := inspectionStatus{ScanID: uuid.NewString(), State: "running"}
		job := &inspectionJob{status: status}
		jobs[status.ScanID] = job
		running = true
		slog.Info("MCP inspection started", "scan_id", status.ScanID, "scope", in.Scope, "namespace", in.Namespace, "pod", in.Pod)
		go func() {
			result, err := scan(ctx, opts)
			mu.Lock()
			defer mu.Unlock()
			running = false
			job.expires = time.Now().Add(time.Hour)
			defer func() { slog.Info("MCP inspection finished", "scan_id", job.status.ScanID, "status", job.status.State) }()
			if err != nil || result == nil {
				job.status.State = "failed"
				job.status.ReasonCode = "INSPECTION_FAILED"
				if errors.Is(err, context.DeadlineExceeded) {
					job.status.ReasonCode = "INSPECTION_TIMEOUT"
				}
				return
			}
			job.result = result
			job.status.State = "completed"
		}()
		return nil, status, nil
	})
	sdk.AddTool(server, &sdk.Tool{Name: "get_inspection_status", Description: "Check inspection status"}, func(_ context.Context, _ *sdk.CallToolRequest, in inspectionID) (*sdk.CallToolResult, inspectionStatus, error) {
		mu.Lock()
		defer mu.Unlock()
		job := jobs[in.ScanID]
		if job == nil || !job.expires.IsZero() && time.Now().After(job.expires) {
			return nil, inspectionStatus{}, errors.New("INSPECTION_REFERENCE_UNAVAILABLE: unknown or expired inspection")
		}
		return nil, job.status, nil
	})
	sdk.AddTool(server, &sdk.Tool{Name: "get_inspection_report", Description: "Read an inspection summary and findings page"}, func(_ context.Context, _ *sdk.CallToolRequest, in inspectionReportRequest) (*sdk.CallToolResult, inspectionReport, error) {
		mu.Lock()
		defer mu.Unlock()
		job := jobs[in.ScanID]
		if job == nil || !job.expires.IsZero() && time.Now().After(job.expires) {
			return nil, inspectionReport{}, errors.New("INSPECTION_REFERENCE_UNAVAILABLE: unknown or expired inspection")
		}
		if job.status.State == "running" {
			return nil, inspectionReport{}, errors.New("INSPECTION_RUNNING: report is pending")
		}
		if job.result == nil {
			return nil, inspectionReport{}, errors.New(job.status.ReasonCode + ": report unavailable")
		}
		findings := job.result.Findings
		if in.Offset < 0 || in.Offset > len(findings) {
			return nil, inspectionReport{}, errors.New("invalid report offset")
		}
		end := min(in.Offset+10, len(findings))
		out := inspectionReport{Scope: job.result.Meta, Summary: job.result.Summary, Score: job.result.HealthScore, Collection: job.result.Collection, Findings: findings[in.Offset:end]}
		if end < len(findings) {
			out.NextOffset = &end
		}
		payload, err := json.Marshal(out)
		if err != nil || len(payload) > 256<<10 {
			return nil, inspectionReport{}, errors.New("INSPECTION_PAGE_TOO_LARGE: report page exceeds size limit")
		}
		return nil, out, nil
	})
	return sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
}

func validInspection(in inspectionRequest) bool {
	switch in.Scope {
	case "cluster":
		return in.Namespace == "" && in.Pod == ""
	case "namespace":
		return len(validation.IsDNS1123Label(in.Namespace)) == 0 && in.Pod == ""
	case "pod":
		return len(validation.IsDNS1123Label(in.Namespace)) == 0 && len(validation.IsDNS1123Subdomain(in.Pod)) == 0
	default:
		return false
	}
}
