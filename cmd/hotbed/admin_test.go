package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminCommands(t *testing.T) {
	t.Parallel()

	type capturedReq struct {
		Method string
		Path   string
		Query  string
		Auth   string
		Body   map[string]any
	}
	var last capturedReq

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &body)
		}
		last = capturedReq{
			Method: r.Method,
			Path:   r.URL.Path,
			Query:  r.URL.RawQuery,
			Auth:   r.Header.Get("Authorization"),
			Body:   body,
		}
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case "/api/admin-agent/status":
			_, _ = w.Write([]byte(`{
				"ok": true,
				"scope": {"controllerDoName": "org:org_alpha", "orgId": "org_alpha", "clusterId": "clu-alpha", "cladefsTenantId": 7},
				"gcWorkerConfigured": true,
				"bindings": ["CONTROLLER", "COMMAND"],
				"controllers": [{"name": "org:org_alpha", "orgKey": "org_alpha", "tenantId": "7"}],
				"recentCommands": [{"commandId": "cmd-1", "doName": "cmd:clu-alpha:cmd-1", "controller": "org:org_alpha"}]
			}`))
		case "/api/admin-agent/schema":
			if r.URL.Query().Get("binding") == "TENANT_REGISTRY" {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"code":"permission_denied","message":"access denied: TenantRegistryDO is global"}`))
				return
			}
			_, _ = w.Write([]byte(`{
				"target": "CONTROLLER(org:org_alpha)",
				"binding": "CONTROLLER",
				"resolvedBy": "name",
				"value": "org:org_alpha",
				"databaseSizeBytes": 65536,
				"tables": [
					{"name": "projects", "type": "table", "rowCount": 2, "columns": [{"cid":0,"name":"id","type":"INTEGER","primaryKey":1},{"cid":1,"name":"name","type":"TEXT"}], "internal": false},
					{"name": "sqlite_master", "type": "table", "rowCount": 5, "columns": [], "internal": true}
				],
				"indexes": [],
				"clusterId": "clu-alpha"
			}`))
		case "/api/admin-agent/sql":
			_, _ = w.Write([]byte(`{
				"target": "CONTROLLER(org:org_alpha)",
				"binding": "CONTROLLER",
				"resolvedBy": "name",
				"value": "org:org_alpha",
				"sql": "SELECT id, name FROM projects",
				"columns": ["id", "name"],
				"rows": [{"id": 1, "name": "hotbed"}, {"id": 2, "name": "cladefs"}],
				"rowCount": 2,
				"truncated": false,
				"rowsRead": 2,
				"rowsWritten": 0,
				"durationMs": 1,
				"databaseSizeBytes": 65536
			}`))
		case "/api/admin-agent/health":
			_, _ = w.Write([]byte(`{
				"ok": true,
				"clusterId": "clu-alpha",
				"orgId": "org_alpha",
				"cladefsTenantId": 7,
				"build": 340,
				"commit": "abcdef1234567890",
				"version": "v0.1.340",
				"connectedWorkers": 3
			}`))
		case "/api/admin-agent/gc":
			_, _ = w.Write([]byte(`{
				"ok": true,
				"tenantId": "7",
				"usage": {
					"cells": 1,
					"totalObjects": 42,
					"totalBytes": 1048576,
					"usage": [{"tenant": "7", "cell": "7/cell-a", "objects": 42, "bytes": 1048576}]
				}
			}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"not_found","message":"unknown path"}`))
		}
	}))
	defer srv.Close()

	runAdmin := func(tok string, stdin string, args ...string) (int, string, string) {
		where := newTestHere(t, map[string]string{"HOTBED_TOKEN": tok})
		var out, errBuf bytes.Buffer
		d := deps{
			here:       where.here(),
			http:       srv.Client(),
			stream:     srv.Client(),
			controller: func() (string, error) { return srv.URL, nil },
			stdin:      strings.NewReader(stdin),
			stdout:     &out,
			stderr:     &errBuf,
		}
		code := run(context.Background(), append([]string{"admin"}, args...), d)
		return code, out.String(), errBuf.String()
	}

	// 1. discover
	code, out, errOut := runAdmin("tok_owner", "", "discover", "--limit", "5")
	if code != exitOK {
		t.Fatalf("admin discover failed: code=%d stderr=%q", code, errOut)
	}
	if last.Path != "/api/admin-agent/status" || last.Query != "limit=5" || last.Auth != "Bearer tok_owner" {
		t.Fatalf("unexpected discover request: %+v", last)
	}
	if !strings.Contains(out, "org_alpha") || !strings.Contains(out, "cmd:clu-alpha:cmd-1") {
		t.Fatalf("unexpected discover output:\n%s", out)
	}

	// 2. schema with --command shorthand and 403 on disallowed TENANT_REGISTRY
	code, out, errOut = runAdmin("tok_owner", "", "schema", "--command", "cmd-1")
	if code != exitOK {
		t.Fatalf("admin schema failed: code=%d stderr=%q", code, errOut)
	}
	if last.Path != "/api/admin-agent/schema" || !strings.Contains(last.Query, "binding=COMMAND") || !strings.Contains(last.Query, "commandId=cmd-1") {
		t.Fatalf("unexpected schema request: %+v", last)
	}
	if !strings.Contains(out, "projects") || strings.Contains(out, "sqlite_master") {
		t.Fatalf("unexpected schema output:\n%s", out)
	}

	code, _, errOut = runAdmin("tok_owner", "", "schema", "-b", "TENANT_REGISTRY")
	if code != exitError || !strings.Contains(errOut, "TenantRegistryDO") {
		t.Fatalf("expected 403 error for TENANT_REGISTRY, got code=%d stderr=%q", code, errOut)
	}

	// 3. sql from stdin with typed --param and --max-rows
	code, out, errOut = runAdmin("tok_owner", "SELECT id, name FROM projects WHERE id = ? AND active = ?", "sql", "-", "--param", "1", "--param", "true", "--max-rows", "25")
	if code != exitOK {
		t.Fatalf("admin sql failed: code=%d stderr=%q", code, errOut)
	}
	if last.Method != http.MethodPost || last.Path != "/api/admin-agent/sql" {
		t.Fatalf("unexpected sql request: %+v", last)
	}
	params, _ := last.Body["params"].([]any)
	if len(params) != 2 || params[0] != float64(1) || params[1] != true {
		t.Fatalf("unexpected typed SQL params: %#v", params)
	}
	if !strings.Contains(out, "hotbed") || !strings.Contains(out, "cladefs") || !strings.Contains(out, "2 rows") {
		t.Fatalf("unexpected sql table output:\n%s", out)
	}

	// 4. sql with --jq and --json
	code, out, errOut = runAdmin("tok_owner", "", "sql", "SELECT * FROM projects", "--jq", ".rows[0].name")
	if code != exitOK {
		t.Fatalf("admin sql --jq failed: code=%d stderr=%q", code, errOut)
	}
	if strings.TrimSpace(out) != "hotbed" {
		t.Fatalf("admin sql --jq = %q, want hotbed", out)
	}
	code, out, errOut = runAdmin("tok_owner", "", "sql", "SELECT * FROM projects", "--json")
	if code != exitOK {
		t.Fatalf("admin sql --json failed: code=%d stderr=%q", code, errOut)
	}
	var env struct {
		OK      bool           `json:"ok"`
		Command string         `json:"command"`
		Data    adminSQLResult `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("decode --json envelope: %v\n%s", err, out)
	}
	if !env.OK || env.Command != "admin sql" || env.Data.RowCount != 2 {
		t.Fatalf("unexpected --json envelope: %+v", env)
	}

	// 5. health
	code, out, errOut = runAdmin("tok_owner", "", "health")
	if code != exitOK {
		t.Fatalf("admin health failed: code=%d stderr=%q", code, errOut)
	}
	if !strings.Contains(out, "cluster=clu-alpha") || !strings.Contains(out, "workers=3") || !strings.Contains(out, "tenant=7") {
		t.Fatalf("unexpected health output:\n%s", out)
	}

	// 6. gc
	code, out, errOut = runAdmin("tok_owner", "", "gc")
	if code != exitOK {
		t.Fatalf("admin gc failed: code=%d stderr=%q", code, errOut)
	}
	if !strings.Contains(out, "tenant=7") || !strings.Contains(out, "7/cell-a") || !strings.Contains(out, "1.0 MiB") {
		t.Fatalf("unexpected gc output:\n%s", out)
	}

	// 7. Terminal control character escaping in SQL cell formatter
	escaped := formatSQLCell("\x1b[31mevil\x1b]0;title\x07")
	if strings.Contains(escaped, "\x1b") || strings.Contains(escaped, "\x07") {
		t.Fatalf("formatSQLCell leaked raw control chars: %q", escaped)
	}
	if !strings.Contains(escaped, `\x1b[31mevil`) {
		t.Fatalf("unexpected escaped SQL cell: %q", escaped)
	}
}
