package api

import (
	"errors"
	"testing"
)

func TestControllerHostPort(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"ubu1", "ubu1:7120"},
		{"ubu1:9000", "ubu1:9000"},
		{"100.73.156.10", "100.73.156.10:7120"},
		{"[fd7a:115c:a1e0::1]:7120", "[fd7a:115c:a1e0::1]:7120"},
		{"fd7a:115c:a1e0::1", "[fd7a:115c:a1e0::1]:7120"},
	} {
		got, err := ControllerHostPort(tc.in)
		if err != nil {
			t.Errorf("ControllerHostPort(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ControllerHostPort(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestControllerHostPortRejects(t *testing.T) {
	for _, in := range []string{"", "http://ubu1", ":7120", "ubu1:", "ubu1:0", "ubu1:70000", "ubu1:http"} {
		_, err := ControllerHostPort(in)
		var e *Error
		if !errors.As(err, &e) || e.Code != CodeInvalidArgs {
			t.Errorf("ControllerHostPort(%q) error = %v, want INVALID_ARGS", in, err)
		}
	}
}
