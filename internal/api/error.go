package api

import "fmt"

// Code names a failure in a way a program can act on. The controller sends
// codes over HTTP and the CLI prints them unchanged, so one set covers both.
type Code string

const (
	CodeInvalidArgs           Code = "INVALID_ARGS"
	CodeAlreadyInitialized    Code = "ALREADY_INITIALIZED"
	CodeNotInitialized        Code = "NOT_INITIALIZED"
	CodeTailscaleUnavailable  Code = "TAILSCALE_UNAVAILABLE"
	CodeControllerUnreachable Code = "CONTROLLER_UNREACHABLE"
	CodeNotAuthorized         Code = "NOT_AUTHORIZED"
	CodeNotJoined             Code = "NOT_JOINED"
	CodeClusterMismatch       Code = "CLUSTER_MISMATCH"
	CodePlatformNotSupported  Code = "PLATFORM_NOT_SUPPORTED"
	CodeNotFound              Code = "NOT_FOUND"
	CodeConflict              Code = "CONFLICT"
	CodeRepoUnreachable       Code = "REPO_UNREACHABLE"
	CodeWorkspaceFailed       Code = "WORKSPACE_FAILED"
	CodeStepFailed            Code = "STEP_FAILED"
	CodeStepNotIdempotent     Code = "STEP_NOT_IDEMPOTENT"
	CodeSyncRefused           Code = "SYNC_REFUSED"
	CodeWorkerUnreachable     Code = "WORKER_UNREACHABLE"
	CodeNodeWaking            Code = "NODE_WAKING" // the node a run goes to is booting; call again
	// CodeCommandStopped is how waiting on a command ends when someone
	// stopped it by hand: its message says who, what they wrote, and how to
	// read its diagnostics.
	CodeCommandStopped Code = "COMMAND_STOPPED"
	// CodeNotReady is how a detached run that waits to be ready (--ready,
	// --ready-port) ends when its command ended first, or did not get ready
	// in time and runs on.
	CodeNotReady Code = "NOT_READY"
	CodeInternal Code = "INTERNAL"
)

// Error is a failure with a code, a hint for what to do next, and whether
// repeating the same request unchanged can succeed.
type Error struct {
	Code       Code   `json:"code"`
	Message    string `json:"message"`
	Suggestion string `json:"suggestion,omitempty"`
	Retryable  bool   `json:"retryable"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Errorf returns an Error with code and a formatted message that is not retryable.
func Errorf(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// WithSuggestion returns e with its suggestion set.
func (e *Error) WithSuggestion(s string) *Error {
	e.Suggestion = s
	return e
}
