package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/itchyny/gojq"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/apicmd"
)

type rawAPICmd struct {
	controllerClient
	Endpoint string   `arg:"" help:"Controller path or ConnectRPC method (e.g. /healthz, ClusterService/GetSnapshot, /hotbed.v1.ClusterService/ListNodes)."`
	Method   string   `short:"X" name:"method" help:"HTTP method (default: POST for ConnectRPC or when body/fields are provided, else GET)."`
	Header   []string `short:"H" name:"header" help:"Add an HTTP request header (Key: Value). Repeatable."`
	RawField []string `short:"f" name:"raw-field" help:"Add a string parameter (key=value). Repeatable."`
	Field    []string `short:"F" name:"field" help:"Add a typed parameter (key=value: bool, number, null, JSON, @file, or string). Repeatable."`
	Data     string   `short:"d" name:"data" help:"Raw request body string."`
	Input    string   `name:"input" help:"File to use as request body ('-' for stdin)."`
	Include  bool     `short:"i" name:"include" help:"Include HTTP response status line and headers in output."`
	Silent   bool     `name:"silent" help:"Do not print the response body."`
}

func (c *rawAPICmd) Exec(context.Context, deps) (any, error) { return nil, nil }

func (c *rawAPICmd) Raw(ctx context.Context, d deps) (int, bool, error) {
	path, rawQuery, isRPC, err := resolveAPIEndpoint(c.Endpoint)
	if err != nil {
		return exitInvalidArgs, false, err
	}
	if (c.Data != "" || c.Input != "") && (len(c.RawField) > 0 || len(c.Field) > 0) {
		return exitInvalidArgs, false, api.Errorf(api.CodeInvalidArgs, "cannot combine --data/--input with -f/--raw-field or -F/--field")
	}
	if c.Data != "" && c.Input != "" {
		return exitInvalidArgs, false, api.Errorf(api.CodeInvalidArgs, "cannot combine --data and --input")
	}

	fields, err := parseAPIFields(c.RawField, c.Field, d.stdin)
	if err != nil {
		return exitInvalidArgs, false, err
	}

	method := strings.ToUpper(strings.TrimSpace(c.Method))
	if method == "" {
		if isRPC || len(fields) > 0 || c.Data != "" || c.Input != "" {
			method = http.MethodPost
		} else {
			method = http.MethodGet
		}
	}

	base, err := api.ControllerBaseURL(c.Controller)
	if err != nil {
		return exitInvalidArgs, false, err
	}
	fullURL := base + path
	if rawQuery != "" {
		fullURL += "?" + rawQuery
	}

	var body []byte
	autoJSON := false
	switch {
	case method == http.MethodGet && len(fields) > 0:
		u, err := url.Parse(fullURL)
		if err != nil {
			return exitInvalidArgs, false, api.Errorf(api.CodeInvalidArgs, "invalid endpoint URL: %v", err)
		}
		q := u.Query()
		for k, v := range fields {
			if v == nil {
				q.Add(k, "")
			} else {
				q.Add(k, fmt.Sprint(v))
			}
		}
		u.RawQuery = q.Encode()
		fullURL = u.String()
	case len(fields) > 0:
		body, err = json.Marshal(fields)
		if err != nil {
			return exitInvalidArgs, false, api.Errorf(api.CodeInvalidArgs, "encode fields: %v", err)
		}
		autoJSON = true
	case c.Input != "":
		if c.Input == "-" {
			body, err = io.ReadAll(d.stdin)
		} else {
			body, err = os.ReadFile(c.Input)
		}
		if err != nil {
			return exitError, false, api.Errorf(api.CodeInvalidArgs, "read --input %q: %v", c.Input, err)
		}
	case c.Data != "":
		body = []byte(c.Data)
	case isRPC && method == http.MethodPost:
		body = []byte("{}")
		autoJSON = true
	}

	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, bodyReader)
	if err != nil {
		return exitInvalidArgs, false, api.Errorf(api.CodeInvalidArgs, "build request: %v", err)
	}

	for _, h := range c.Header {
		k, v, ok := strings.Cut(h, ":")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return exitInvalidArgs, false, api.Errorf(api.CodeInvalidArgs, "invalid header %q (expected Key: Value)", h)
		}
		req.Header.Add(k, strings.TrimSpace(v))
	}
	if req.Header.Get("Authorization") == "" {
		if tok := controllerAuthToken(d.getenv, c.Controller); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
			if host, _ := os.Hostname(); host != "" && req.Header.Get("X-Hotbed-Host") == "" {
				req.Header.Set("X-Hotbed-Host", host)
			}
		}
	}
	if req.Header.Get("Content-Type") == "" && (autoJSON || isRPC || (len(body) > 0 && json.Valid(body))) {
		req.Header.Set("Content-Type", "application/json")
	}

	hc := d.stream
	if hc == nil {
		hc = d.http
	}
	resp, err := hc.Do(req)
	if err != nil {
		return exitError, false, api.Errorf(api.CodeControllerUnreachable, "%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return exitError, false, api.Errorf(api.CodeInternal, "read response from %s %s: %v", method, path, err)
	}

	printed := false
	if c.Include {
		fmt.Fprintf(d.stdout, "%s %s\n", resp.Proto, resp.Status)
		keys := make([]string, 0, len(resp.Header))
		for k := range resp.Header {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			for _, v := range resp.Header[k] {
				fmt.Fprintf(d.stdout, "%s: %s\n", k, v)
			}
		}
		_, _ = io.WriteString(d.stdout, "\n")
		printed = true
	}

	if !c.Silent && len(respBody) > 0 {
		if d.output.JQ != "" {
			q, err := gojq.Parse(d.output.JQ)
			if err != nil {
				return exitInvalidArgs, printed, api.Errorf(api.CodeInvalidArgs, "invalid --jq expression: %v", err)
			}
			var val any
			if err := json.Unmarshal(respBody, &val); err != nil {
				return exitError, printed, api.Errorf(api.CodeInternal, "response is not valid JSON for --jq: %v", err)
			}
			if err := apicmd.FormatJQ(d.stdout, q, val); err != nil {
				return exitError, printed, err
			}
			printed = true
		} else {
			if _, err := d.stdout.Write(respBody); err != nil {
				return exitError, printed, err
			}
			printed = true
		}
	}

	if resp.StatusCode >= 400 {
		return exitError, printed, api.Errorf(api.CodeInternal, "%s %s: %s", method, path, resp.Status)
	}
	return exitOK, printed, nil
}

// resolveAPIEndpoint normalizes an endpoint argument into a URL path, optional
// raw query string, and whether it targets a ConnectRPC service method.
func resolveAPIEndpoint(endpoint string) (path, rawQuery string, isRPC bool, err error) {
	ep := strings.TrimSpace(endpoint)
	if ep == "" {
		return "", "", false, api.Errorf(api.CodeInvalidArgs, "endpoint is required")
	}
	if strings.Contains(ep, "://") {
		return "", "", false, api.Errorf(api.CodeInvalidArgs, "endpoint %q must be a path or Service/Method, not a full URL", ep).
			WithSuggestion("pass a path like /healthz or ConnectRPC method like ClusterService/GetSnapshot")
	}
	ep, rawQuery, _ = strings.Cut(ep, "?")
	trimmed := strings.TrimPrefix(ep, "/")
	if trimmed == "" {
		return "/", rawQuery, false, nil
	}
	if svc, method, ok := strings.Cut(trimmed, "/"); ok && method != "" && !strings.Contains(method, "/") && strings.HasSuffix(svc, "Service") {
		if !strings.Contains(svc, ".") {
			return "/hotbed.v1." + svc + "/" + method, rawQuery, true, nil
		}
		return "/" + svc + "/" + method, rawQuery, true, nil
	}
	return "/" + trimmed, rawQuery, false, nil
}

func parseAPIFields(rawFields, typedFields []string, stdin io.Reader) (map[string]any, error) {
	if len(rawFields) == 0 && len(typedFields) == 0 {
		return nil, nil
	}
	out := make(map[string]any, len(rawFields)+len(typedFields))
	for _, f := range rawFields {
		k, v, ok := strings.Cut(f, "=")
		if !ok || k == "" {
			return nil, api.Errorf(api.CodeInvalidArgs, "invalid raw field %q (expected key=value)", f)
		}
		out[k] = v
	}
	for _, f := range typedFields {
		k, v, ok := strings.Cut(f, "=")
		if !ok || k == "" {
			return nil, api.Errorf(api.CodeInvalidArgs, "invalid field %q (expected key=value)", f)
		}
		val, err := parseTypedFieldValue(v, stdin)
		if err != nil {
			return nil, api.Errorf(api.CodeInvalidArgs, "field %q: %v", k, err)
		}
		out[k] = val
	}
	return out, nil
}

func parseTypedFieldValue(v string, stdin io.Reader) (any, error) {
	if v == "@-" {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return nil, err
		}
		return string(b), nil
	}
	if strings.HasPrefix(v, "@") {
		b, err := os.ReadFile(v[1:])
		if err != nil {
			return nil, err
		}
		return string(b), nil
	}
	switch v {
	case "true":
		return true, nil
	case "false":
		return false, nil
	case "null":
		return nil, nil
	}
	if len(v) > 0 && (v[0] == '{' || v[0] == '[' || v[0] == '-' || (v[0] >= '0' && v[0] <= '9')) {
		var parsed any
		if err := json.Unmarshal([]byte(v), &parsed); err == nil {
			return parsed, nil
		}
	}
	return v, nil
}
