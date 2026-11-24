package controller

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect/v2"

	"github.com/semistrict/hotbed/internal/api"
	controllerdb "github.com/semistrict/hotbed/internal/controller/db/gen"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// MigrationState is the persisted redirect state once this controller has
// migrated its cluster state to another controller.
type MigrationState struct {
	TargetController string
	ControllerNodeID string
	DefaultToken     string
	WorkerTokens     map[string]string
	MigratedAt       time.Time
}

// GetControllerMigration returns the active migration redirect state, or nil
// when this controller has not been migrated.
func (s *Store) GetControllerMigration(ctx context.Context) (*MigrationState, error) {
	row, err := s.q.GetControllerMigration(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	tokens := map[string]string{}
	if row.WorkerTokensJson != "" {
		_ = json.Unmarshal([]byte(row.WorkerTokensJson), &tokens)
	}
	return &MigrationState{
		TargetController: row.TargetController,
		ControllerNodeID: row.ControllerNodeID,
		DefaultToken:     row.DefaultToken,
		WorkerTokens:     tokens,
		MigratedAt:       time.UnixMilli(row.MigratedAt),
	}, nil
}

// SetControllerMigration persists this controller's migrated redirect state.
func (s *Store) SetControllerMigration(ctx context.Context, st MigrationState) error {
	tokJSON, err := json.Marshal(st.WorkerTokens)
	if err != nil {
		return err
	}
	if st.WorkerTokens == nil {
		tokJSON = []byte("{}")
	}
	return s.q.SetControllerMigration(ctx, controllerdb.SetControllerMigrationParams{
		TargetController: st.TargetController,
		ControllerNodeID: st.ControllerNodeID,
		DefaultToken:     st.DefaultToken,
		WorkerTokensJson: string(tokJSON),
		MigratedAt:       st.MigratedAt.UnixMilli(),
	})
}

// migrationExportTables lists the durable controller tables exported to
// POST /admin/import on a target controller, in FK dependency order.
var migrationExportTables = []struct {
	name    string
	columns []string
}{
	{"nodes", []string{"id", "hostname", "login", "addr", "joined_at", "last_seen", "epoch", "applied", "applied_at", "heartbeat", "retired_at"}},
	{"repos", []string{"id", "doc"}},
	{"projects", []string{"id", "name", "created_at", "setup"}},
	{"project_repos", []string{"project_id", "remote"}},
	{"project_changes", []string{"seq", "project_id", "what", "repo", "at", "session", "host", "reason"}},
	{"sessions", []string{"id", "agent", "project_id", "t3code_thread_id", "started_at", "last_active_at", "ended_at", "end_reason"}},
	{"workspaces", []string{"id", "session_id", "node_id", "agent", "created_at", "fork_millis", "released_at"}},
	{"commands", []string{"id", "session_id", "node_id", "workspace_id", "created_at"}},
	{"session_events", []string{"seq", "session_id", "doc"}},
	{"history", []string{"id", "node_id", "repo_id", "ended_at", "doc", "project_id", "session_id"}},
	{"command_diagnostics", []string{"command_id", "seq", "node_id", "created_at", "doc"}},
	{"placements", []string{"id", "at", "session_id", "doc"}},
	{"schedules", []string{"name", "project_id", "doc", "next_at"}},
	{"schedule_runs", []string{"id", "schedule", "session_id", "command_id", "ended_at", "doc"}},
	{"questions", []string{"id", "project_id", "repo", "state", "rank", "asked_by", "asked_at", "updated_at", "doc"}},
	{"question_changes", []string{"seq", "question_id", "what", "at", "session", "host", "reason", "doc"}},
	{"reviews", []string{"id", "project_id", "state", "requested_by", "requested_at", "updated_at", "doc"}},
	{"review_findings", []string{"id", "review_id", "doc"}},
	{"review_events", []string{"seq", "review_id", "finding_id", "what", "at", "session", "host", "quick", "text", "doc"}},
	{"git_credentials", []string{"name", "host", "doc", "secret"}},
}

// ExportSnapshot reads all durable tables from hotbed.db inside a consistent
// read transaction in the exact JSON shape expected by POST /admin/import.
func (s *Store) ExportSnapshot(ctx context.Context, clusterID string) (map[string]any, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin snapshot read: %w", err)
	}
	defer tx.Rollback()

	tables := make(map[string][]map[string]any, len(migrationExportTables))
	for _, spec := range migrationExportTables {
		query := fmt.Sprintf("SELECT %s FROM %s", strings.Join(spec.columns, ", "), spec.name)
		rows, err := tx.QueryContext(ctx, query)
		if err != nil {
			return nil, fmt.Errorf("export %s: %w", spec.name, err)
		}
		var out []map[string]any
		for rows.Next() {
			vals := make([]any, len(spec.columns))
			ptrs := make([]any, len(spec.columns))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan %s: %w", spec.name, err)
			}
			rec := make(map[string]any, len(spec.columns))
			for i, col := range spec.columns {
				switch v := vals[i].(type) {
				case []byte:
					rec[col] = string(v)
				default:
					rec[col] = v
				}
			}
			out = append(out, rec)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("iterate %s: %w", spec.name, err)
		}
		rows.Close()
		if len(out) > 0 {
			tables[spec.name] = out
		}
	}
	return map[string]any{
		"clusterId": clusterID,
		"tables":    tables,
	}, nil
}

var migratedAllowedProcedures = map[string]bool{
	"/hotbed.v1.ClusterService/Heartbeat":         true,
	"/hotbed.v1.ClusterService/PushEvents":        true,
	"/hotbed.v1.ClusterService/ListNodes":         true,
	"/hotbed.v1.ClusterService/GetSnapshot":       true,
	"/hotbed.v1.ClusterService/GetBuild":          true,
	"/hotbed.v1.ClusterService/MigrateController": true,
}

func (s *Server) migratedInterceptor() connect.ServerInterceptor {
	return func(next connect.ServerFunc) connect.ServerFunc {
		return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
			if !migratedAllowedProcedures[spec.Procedure] {
				if mig, err := s.cfg.Store.GetControllerMigration(ctx); err == nil && mig != nil {
					return api.Errorf(api.CodeConflict, "this controller has migrated to %s", mig.TargetController).
						WithSuggestion(fmt.Sprintf("point your client to %s (HOTBED_CONTROLLER=%s)", mig.TargetController, mig.TargetController))
				}
			}
			return next(ctx, spec, stream)
		}
	}
}

func (s *Server) migrationForWorker(ctx context.Context, nodeID string) *pb.ControllerMigration {
	mig, err := s.cfg.Store.GetControllerMigration(ctx)
	if err != nil || mig == nil {
		return nil
	}
	tok := mig.WorkerTokens[nodeID]
	if tok == "" {
		tok = mig.DefaultToken
	}
	return &pb.ControllerMigration{
		Controller:       mig.TargetController,
		Token:            tok,
		ControllerNodeId: mig.ControllerNodeID,
	}
}

// MigrateController exports durable cluster state to req.TargetController's
// /admin/import endpoint (unless SkipExport is set) and records migrated state
// so workers automatically redirect and reconnect to the target controller.
func (s *Server) MigrateController(ctx context.Context, req *pb.MigrateControllerRequest) (*pb.MigrateControllerResponse, error) {
	rawTarget := strings.TrimSpace(req.GetTargetController())
	if rawTarget == "" {
		return nil, api.Errorf(api.CodeInvalidArgs, "target_controller is required")
	}
	targetURL, err := api.ControllerBaseURL(rawTarget)
	if err != nil {
		return nil, err
	}

	var (
		importedTables   int32
		importedRows     int32
		controllerNodeID = "cf:" + s.cfg.Cluster.ClusterID
		workerTokens     = map[string]string{}
	)

	if !req.GetSkipExport() {
		snap, err := s.cfg.Store.ExportSnapshot(ctx, s.cfg.Cluster.ClusterID)
		if err != nil {
			return nil, api.Errorf(api.CodeInternal, "export controller state: %v", err)
		}
		snap["force"] = req.GetForce()
		payload, err := json.Marshal(snap)
		if err != nil {
			return nil, api.Errorf(api.CodeInternal, "encode controller snapshot: %v", err)
		}
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL+"/admin/import", bytes.NewReader(payload))
		if err != nil {
			return nil, api.Errorf(api.CodeInvalidArgs, "build import request: %v", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		if tok := strings.TrimSpace(req.GetTargetToken()); tok != "" {
			httpReq.Header.Set("Authorization", "Bearer "+tok)
		}
		hc := s.cfg.GitAuthHTTP
		if hc == nil {
			hc = &http.Client{Timeout: 2 * time.Minute}
		}
		resp, err := hc.Do(httpReq)
		if err != nil {
			return nil, api.Errorf(api.CodeControllerUnreachable, "reach target controller %s: %v", targetURL, err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if resp.StatusCode != http.StatusOK {
			code := api.CodeInternal
			if resp.StatusCode == http.StatusConflict {
				code = api.CodeConflict
			} else if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
				code = api.CodeNotAuthorized
			}
			return nil, api.Errorf(code, "target controller %s/admin/import returned %s: %s", targetURL, resp.Status, strings.TrimSpace(string(body)))
		}
		var res struct {
			ControllerNodeID string            `json:"controllerNodeId"`
			ImportedTables   int32             `json:"importedTables"`
			ImportedRows     int32             `json:"importedRows"`
			WorkerTokens     map[string]string `json:"workerTokens"`
		}
		if err := json.Unmarshal(body, &res); err != nil {
			return nil, api.Errorf(api.CodeInternal, "parse target import response: %v", err)
		}
		if res.ControllerNodeID != "" {
			controllerNodeID = res.ControllerNodeID
		}
		importedTables = res.ImportedTables
		importedRows = res.ImportedRows
		if res.WorkerTokens != nil {
			workerTokens = res.WorkerTokens
		}
	}

	targetStored := rawTarget
	if api.IsRemoteURL(rawTarget) {
		targetStored = targetURL
	}
	if err := s.cfg.Store.SetControllerMigration(ctx, MigrationState{
		TargetController: targetStored,
		ControllerNodeID: controllerNodeID,
		DefaultToken:     strings.TrimSpace(req.GetWorkerToken()),
		WorkerTokens:     workerTokens,
		MigratedAt:       s.now(),
	}); err != nil {
		return nil, api.Errorf(api.CodeInternal, "record migrated state: %v", err)
	}
	slog.Info("controller entered migrated state; redirecting workers",
		"target", targetStored, "controllerNodeId", controllerNodeID,
		"importedTables", importedTables, "importedRows", importedRows, "mintedTokens", len(workerTokens))

	return &pb.MigrateControllerResponse{
		ClusterId:          s.cfg.Cluster.ClusterID,
		TargetController:   targetStored,
		ControllerNodeId:   controllerNodeID,
		ImportedTables:     importedTables,
		ImportedRows:       importedRows,
		MintedWorkerTokens: int32(len(workerTokens)),
	}, nil
}
