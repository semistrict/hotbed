package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/semistrict/hotbed/internal/api"
)

type adminCmd struct {
	Discover adminDiscoverCmd `cmd:"" aliases:"status,ls" help:"Discover this organization's Cloudflare Durable Objects, scope, allowed bindings, and recent commands."`
	Schema   adminSchemaCmd   `cmd:"" help:"Inspect the SQLite schema, tables, row counts, indexes, and DB size of an org-scoped Durable Object."`
	SQL      adminSQLCmd      `cmd:"" name:"sql" help:"Run a read-only SQLite query (SELECT / WITH / EXPLAIN / read-only PRAGMA) on an org-scoped Durable Object."`
	Health   adminHealthCmd   `cmd:"" help:"Fetch live health for this organization's Controller Durable Object."`
	GC       adminGCCmd       `cmd:"" name:"gc" help:"Inspect CladeFS R2 storage usage and GC dry-run/live status strictly for this organization's tenant ID."`
}

// ---- admin discover ----

type adminDiscoverCmd struct {
	controllerClient
	Limit int `default:"15" help:"Max recent Command Durable Objects to include (1-50)."`
}

type adminOrgScope struct {
	ControllerDOName string  `json:"controllerDoName"`
	OrgID            *string `json:"orgId"`
	ClusterID        *string `json:"clusterId"`
	CladeFSTenantID  *int64  `json:"cladefsTenantId"`
}

type adminControllerEntry struct {
	Name     string  `json:"name"`
	OrgKey   string  `json:"orgKey"`
	TenantID *string `json:"tenantId"`
}

type adminRecentCommand struct {
	CommandID  string `json:"commandId"`
	DOName     string `json:"doName"`
	Controller string `json:"controller"`
}

type adminDiscoverResult struct {
	OK                 bool                   `json:"ok"`
	Scope              adminOrgScope          `json:"scope"`
	GCWorkerConfigured bool                   `json:"gcWorkerConfigured"`
	Bindings           []string               `json:"bindings"`
	Controllers        []adminControllerEntry `json:"controllers"`
	RecentCommands     []adminRecentCommand   `json:"recentCommands"`
}

func (c *adminDiscoverCmd) Exec(ctx context.Context, d deps) (any, error) {
	q := url.Values{}
	if c.Limit > 0 {
		q.Set("limit", strconv.Itoa(c.Limit))
	}
	path := "/api/admin-agent/status"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var out adminDiscoverResult
	if err := c.controllerJSON(ctx, d, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ---- admin schema ----

type adminSchemaCmd struct {
	controllerClient
	Binding string `short:"b" help:"Durable Object binding: CONTROLLER, COMMAND, or WORKER (defaults to CONTROLLER, or COMMAND with --command)."`
	Name    string `help:"Durable Object logical name (e.g. default, org:<id>, cmd:<clusterId>:<cmdId>)."`
	ID      string `help:"64-character hex Cloudflare Durable Object ID."`
	Command string `help:"Shorthand: inspect the Command Durable Object for this command ID within your org."`
}

type adminTableColumn struct {
	CID          int     `json:"cid"`
	Name         string  `json:"name"`
	Type         string  `json:"type"`
	NotNull      bool    `json:"notNull"`
	DefaultValue *string `json:"defaultValue"`
	PrimaryKey   int     `json:"primaryKey"`
}

type adminTableInfo struct {
	Name     string             `json:"name"`
	Type     string             `json:"type"`
	RowCount *int64             `json:"rowCount"`
	Columns  []adminTableColumn `json:"columns"`
	SQL      string             `json:"sql"`
	Internal bool               `json:"internal"`
}

type adminIndexInfo struct {
	Name      string `json:"name"`
	TableName string `json:"tableName"`
	SQL       string `json:"sql"`
}

type adminSchemaResult struct {
	Target            string           `json:"target"`
	Binding           string           `json:"binding"`
	ResolvedBy        string           `json:"resolvedBy"`
	Value             string           `json:"value"`
	DatabaseSizeBytes int64            `json:"databaseSizeBytes"`
	AlarmAtMs         *int64           `json:"alarmAtMs"`
	Tables            []adminTableInfo `json:"tables"`
	Indexes           []adminIndexInfo `json:"indexes"`
	ClusterID         string           `json:"clusterId,omitempty"`
	OrgID             string           `json:"orgId,omitempty"`
	CladeFSTenantID   *string          `json:"cladefsTenantId,omitempty"`
}

func (c *adminSchemaCmd) Exec(ctx context.Context, d deps) (any, error) {
	q := url.Values{}
	binding := strings.ToUpper(strings.TrimSpace(c.Binding))
	cmdID := strings.TrimSpace(c.Command)
	if binding == "" {
		if cmdID != "" {
			binding = "COMMAND"
		} else {
			binding = "CONTROLLER"
		}
	}
	q.Set("binding", binding)
	if cmdID != "" {
		q.Set("commandId", cmdID)
	}
	if name := strings.TrimSpace(c.Name); name != "" {
		q.Set("name", name)
	}
	if id := strings.TrimSpace(c.ID); id != "" {
		q.Set("id", id)
	}
	var out adminSchemaResult
	if err := c.controllerJSON(ctx, d, http.MethodGet, "/api/admin-agent/schema?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ---- admin sql ----

type adminSQLCmd struct {
	controllerClient
	Query   string   `arg:"" help:"Read-only SQLite query (pass '-' to read from stdin)."`
	Binding string   `short:"b" help:"Durable Object binding: CONTROLLER, COMMAND, or WORKER (defaults to CONTROLLER, or COMMAND with --command)."`
	Name    string   `help:"Durable Object logical name."`
	ID      string   `help:"64-character hex Cloudflare Durable Object ID."`
	Command string   `help:"Shorthand: query the Command Durable Object for this command ID within your org."`
	Params  []string `name:"param" short:"p" help:"Positional '?' SQL bind parameter (JSON number/bool/null or string). Repeatable."`
	MaxRows int      `name:"max-rows" default:"100" help:"Maximum rows to return (1-500)."`
}

type adminSQLResult struct {
	Target            string           `json:"target"`
	Binding           string           `json:"binding"`
	ResolvedBy        string           `json:"resolvedBy"`
	Value             string           `json:"value"`
	SQL               string           `json:"sql"`
	Columns           []string         `json:"columns"`
	Rows              []map[string]any `json:"rows"`
	RowCount          int              `json:"rowCount"`
	Truncated         bool             `json:"truncated"`
	RowsRead          int64            `json:"rowsRead"`
	RowsWritten       int64            `json:"rowsWritten"`
	DurationMs        float64          `json:"durationMs"`
	DatabaseSizeBytes int64            `json:"databaseSizeBytes"`
}

func parseSQLParam(raw string) any {
	trimmed := strings.TrimSpace(raw)
	switch trimmed {
	case "null":
		return nil
	case "true":
		return true
	case "false":
		return false
	}
	if trimmed != "" {
		if i, err := strconv.ParseInt(trimmed, 10, 64); err == nil && strconv.FormatInt(i, 10) == trimmed {
			return i
		}
		if f, err := strconv.ParseFloat(trimmed, 64); err == nil && strings.ContainsAny(trimmed, ".eE") {
			return f
		}
	}
	return raw
}

func (c *adminSQLCmd) Exec(ctx context.Context, d deps) (any, error) {
	sqlText := c.Query
	if sqlText == "-" {
		b, err := io.ReadAll(d.stdin)
		if err != nil {
			return nil, fmt.Errorf("read SQL from stdin: %w", err)
		}
		sqlText = string(b)
	}
	sqlText = strings.TrimSpace(sqlText)
	if sqlText == "" {
		return nil, api.Errorf(api.CodeInvalidArgs, "SQL query must not be empty")
	}
	cmdID := strings.TrimSpace(c.Command)
	binding := strings.ToUpper(strings.TrimSpace(c.Binding))
	if binding == "" {
		if cmdID != "" {
			binding = "COMMAND"
		} else {
			binding = "CONTROLLER"
		}
	}
	payload := map[string]any{
		"binding": binding,
		"sql":     sqlText,
	}
	if cmdID != "" {
		payload["commandId"] = cmdID
	}
	if name := strings.TrimSpace(c.Name); name != "" {
		payload["name"] = name
	}
	if id := strings.TrimSpace(c.ID); id != "" {
		payload["id"] = id
	}
	if len(c.Params) > 0 {
		params := make([]any, len(c.Params))
		for i, p := range c.Params {
			params[i] = parseSQLParam(p)
		}
		payload["params"] = params
	}
	if c.MaxRows > 0 {
		payload["maxRows"] = c.MaxRows
	}
	var out adminSQLResult
	if err := c.controllerJSON(ctx, d, http.MethodPost, "/api/admin-agent/sql", payload, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ---- admin health ----

type adminHealthCmd struct {
	controllerClient
}

type adminHealthResult struct {
	OK               bool   `json:"ok"`
	Controller       string `json:"controller,omitempty"`
	ClusterID        string `json:"clusterId"`
	OrgID            string `json:"orgId,omitempty"`
	CladeFSTenantID  *int64 `json:"cladefsTenantId,omitempty"`
	Build            int64  `json:"build,omitempty"`
	Commit           string `json:"commit,omitempty"`
	Version          string `json:"version,omitempty"`
	ConnectedWorkers int    `json:"connectedWorkers"`
}

func (c *adminHealthCmd) Exec(ctx context.Context, d deps) (any, error) {
	var out adminHealthResult
	if err := c.controllerJSON(ctx, d, http.MethodGet, "/api/admin-agent/health", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ---- admin gc ----

type adminGCCmd struct {
	controllerClient
	Action string `default:"status" enum:"status,dry_run_cell,live_cell" help:"Inspection action: status, dry_run_cell, or live_cell."`
	Cell   string `help:"Cell ID (required for dry_run_cell and live_cell)."`
}

type adminGCResult struct {
	OK       bool           `json:"ok"`
	TenantID string         `json:"tenantId"`
	Cell     string         `json:"cell,omitempty"`
	Status   int            `json:"status,omitempty"`
	Health   map[string]any `json:"health,omitempty"`
	Usage    map[string]any `json:"usage,omitempty"`
	Result   any            `json:"result,omitempty"`
}

func (c *adminGCCmd) Exec(ctx context.Context, d deps) (any, error) {
	q := url.Values{}
	q.Set("action", c.Action)
	if cell := strings.TrimSpace(c.Cell); cell != "" {
		q.Set("cell", cell)
	}
	var out adminGCResult
	if err := c.controllerJSON(ctx, d, http.MethodGet, "/api/admin-agent/gc?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ---- Plain-text formatting for hotbed admin ----

func formatAdminReport(data any) (string, bool) {
	switch v := data.(type) {
	case adminDiscoverResult:
		return formatAdminDiscover(v), true
	case adminSchemaResult:
		return formatAdminSchema(v), true
	case adminSQLResult:
		return formatAdminSQL(v), true
	case adminHealthResult:
		return formatAdminHealth(v), true
	case adminGCResult:
		return formatAdminGC(v), true
	default:
		return "", false
	}
}

func formatAdminDiscover(r adminDiscoverResult) string {
	var b strings.Builder
	scopeLabel := r.Scope.ControllerDOName
	if r.Scope.OrgID != nil && *r.Scope.OrgID != "" {
		scopeLabel = fmt.Sprintf("%s (%s)", *r.Scope.OrgID, r.Scope.ControllerDOName)
	}
	fmt.Fprintf(&b, "scope:    %s\n", scopeLabel)
	if r.Scope.ClusterID != nil && *r.Scope.ClusterID != "" {
		fmt.Fprintf(&b, "cluster:  %s\n", *r.Scope.ClusterID)
	}
	if r.Scope.CladeFSTenantID != nil {
		fmt.Fprintf(&b, "tenant:   %d\n", *r.Scope.CladeFSTenantID)
	}
	fmt.Fprintf(&b, "bindings: %s\n", strings.Join(r.Bindings, ", "))
	fmt.Fprintf(&b, "gc:       %v\n", r.GCWorkerConfigured)
	if len(r.RecentCommands) > 0 {
		b.WriteString("\nRecent Command Durable Objects:\n")
		tw := tabwriter.NewWriter(&b, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "COMMAND\tDO NAME")
		for _, c := range r.RecentCommands {
			fmt.Fprintf(tw, "%s\t%s\n", c.CommandID, c.DOName)
		}
		_ = tw.Flush()
	}
	return b.String()
}

func formatAdminSchema(r adminSchemaResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  size=%s", r.Target, formatBytes(r.DatabaseSizeBytes))
	if r.ClusterID != "" {
		fmt.Fprintf(&b, "  cluster=%s", r.ClusterID)
	}
	if r.AlarmAtMs != nil {
		fmt.Fprintf(&b, "  alarm=%d", *r.AlarmAtMs)
	}
	b.WriteByte('\n')

	var userTables []adminTableInfo
	for _, t := range r.Tables {
		if !t.Internal {
			userTables = append(userTables, t)
		}
	}
	if len(userTables) == 0 {
		b.WriteString("(no user tables)\n")
		return b.String()
	}
	tw := tabwriter.NewWriter(&b, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "TABLE\tROWS\tCOLUMNS")
	for _, t := range userTables {
		rows := "-"
		if t.RowCount != nil {
			rows = strconv.FormatInt(*t.RowCount, 10)
		}
		cols := make([]string, 0, len(t.Columns))
		for _, col := range t.Columns {
			s := col.Name
			if col.Type != "" {
				s += ":" + col.Type
			}
			if col.PrimaryKey > 0 {
				s += "*"
			}
			cols = append(cols, s)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", t.Name, rows, strings.Join(cols, ", "))
	}
	_ = tw.Flush()
	return b.String()
}

// sanitizeTerminalCell escapes ASCII and C1 control characters so untrusted
// SQLite values cannot inject ANSI/OSC terminal escape sequences.
func sanitizeTerminalCell(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteByte(' ')
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func formatSQLCell(v any) string {
	if v == nil {
		return "NULL"
	}
	switch x := v.(type) {
	case string:
		s := sanitizeTerminalCell(x)
		if len(s) > 80 {
			return s[:77] + "..."
		}
		return s
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return sanitizeTerminalCell(fmt.Sprint(x))
		}
		s := sanitizeTerminalCell(string(b))
		if len(s) > 80 {
			return s[:77] + "..."
		}
		return s
	}
}

func formatAdminSQL(r adminSQLResult) string {
	var b strings.Builder
	if len(r.Columns) > 0 && len(r.Rows) > 0 {
		tw := tabwriter.NewWriter(&b, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, strings.Join(r.Columns, "\t"))
		for _, row := range r.Rows {
			vals := make([]string, len(r.Columns))
			for i, col := range r.Columns {
				vals[i] = formatSQLCell(row[col])
			}
			fmt.Fprintln(tw, strings.Join(vals, "\t"))
		}
		_ = tw.Flush()
	} else {
		b.WriteString("(0 rows)\n")
	}
	capped := ""
	if r.Truncated {
		capped = " (capped)"
	}
	fmt.Fprintf(&b, "[%s · %d rows%s · %d read · %.0fms]\n", r.Target, r.RowCount, capped, r.RowsRead, r.DurationMs)
	return b.String()
}

func formatAdminHealth(r adminHealthResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "ok=%v  cluster=%s  workers=%d", r.OK, r.ClusterID, r.ConnectedWorkers)
	if r.OrgID != "" {
		fmt.Fprintf(&b, "  org=%s", r.OrgID)
	}
	if r.CladeFSTenantID != nil {
		fmt.Fprintf(&b, "  tenant=%d", *r.CladeFSTenantID)
	}
	if r.Version != "" {
		fmt.Fprintf(&b, "  version=%s", r.Version)
	}
	if r.Build > 0 {
		fmt.Fprintf(&b, "  build=%d", r.Build)
	}
	if r.Commit != "" {
		commit := r.Commit
		if len(commit) > 12 {
			commit = commit[:12]
		}
		fmt.Fprintf(&b, "  commit=%s", commit)
	}
	b.WriteByte('\n')
	return b.String()
}

func formatAdminGC(r adminGCResult) string {
	if r.Result != nil {
		b, err := json.MarshalIndent(map[string]any{
			"ok":       r.OK,
			"tenantId": r.TenantID,
			"cell":     r.Cell,
			"result":   r.Result,
		}, "", "  ")
		if err != nil {
			return fmt.Sprintf("%v\n", r.Result)
		}
		return string(b) + "\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "tenant=%s  ok=%v", r.TenantID, r.OK)
	if r.Usage != nil {
		if cells, ok := r.Usage["cells"].(float64); ok {
			fmt.Fprintf(&b, "  cells=%.0f", cells)
		}
		if objs, ok := r.Usage["totalObjects"].(float64); ok {
			fmt.Fprintf(&b, "  objects=%.0f", objs)
		}
		if tb, ok := r.Usage["totalBytes"].(float64); ok {
			fmt.Fprintf(&b, "  bytes=%s", formatBytes(int64(tb)))
		}
	}
	b.WriteByte('\n')
	if r.Usage != nil {
		if list, ok := r.Usage["usage"].([]any); ok && len(list) > 0 {
			tw := tabwriter.NewWriter(&b, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "CELL\tOBJECTS\tBYTES")
			for _, item := range list {
				m, _ := item.(map[string]any)
				if m == nil {
					continue
				}
				cell, _ := m["cell"].(string)
				objs := "-"
				if v, ok := m["objects"].(float64); ok {
					objs = strconv.FormatInt(int64(v), 10)
				}
				sz := "-"
				if v, ok := m["bytes"].(float64); ok {
					sz = formatBytes(int64(v))
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\n", cell, objs, sz)
			}
			_ = tw.Flush()
		}
	}
	return b.String()
}
