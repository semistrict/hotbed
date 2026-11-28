package guestd

import (
	"errors"
	"io"
	"syscall"
	"testing"

	"connectrpc.com/connect/v2"
)

func TestIsTransientResetMatchesConnectUnavailableEOF(t *testing.T) {
	for _, err := range []error{
		io.EOF,
		io.ErrUnexpectedEOF,
		syscall.ECONNRESET,
		connect.NewError(connect.CodeUnavailable, "unexpected EOF"),
		connect.NewError(connect.CodeUnavailable, "read: connection reset by peer"),
		connect.NewError(connect.CodeUnavailable, "write: broken pipe"),
	} {
		if !isTransientReset(err) {
			t.Errorf("isTransientReset(%v) = false, want true", err)
		}
	}
	for _, err := range []error{
		nil,
		errors.New("plain error"),
		connect.NewError(connect.CodeNotFound, "command not found"),
		connect.NewError(connect.CodeInvalidArgument, "unexpected EOF"),
	} {
		if isTransientReset(err) {
			t.Errorf("isTransientReset(%v) = true, want false", err)
		}
	}
}
