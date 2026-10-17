package ghcredentials

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// HostResult separates an unavailable authentication source from a broken
// transport. Only Accounts contains secrets; Status and Decisions are safe for
// the local diagnostic recorder.
type HostResult struct {
	Accounts   []Account      `json:"accounts"`
	Status     string         `json:"status"`
	Decisions  []hostDecision `json:"decisions,omitempty"`
	Context    HostContext    `json:"context"`
	DurationMS int64          `json:"duration_ms"`
}

type HostContext struct {
	Binary    string            `json:"binary,omitempty"`
	ConfigDir string            `json:"config_dir,omitempty"`
	Inputs    map[string]string `json:"inputs,omitempty"`
}

type boundaryError struct {
	operation, kind string
	cause           error
}

func (e *boundaryError) Error() string { return e.operation + ": " + e.kind }
func (e *boundaryError) Unwrap() error { return e.cause }

func boundary(operation string, err error) error {
	if err == nil {
		return nil
	}
	return &boundaryError{operation: operation, kind: errorKind(err), cause: err}
}

func errorKind(err error) string {
	classes := []struct {
		cause error
		kind  string
	}{
		{context.DeadlineExceeded, "timeout"},
		{context.Canceled, "cancelled"},
		{os.ErrPermission, "permission_denied"},
		{os.ErrNotExist, "not_found"},
		{syscall.ECONNREFUSED, "connection_refused"},
		{errInvalidConfig, "schema_invalid"},
	}
	for _, class := range classes {
		if errors.Is(err, class.cause) {
			return class.kind
		}
	}
	if _, ok := errors.AsType[*exec.ExitError](err); ok {
		return "process_exit"
	}
	return "io_error"
}

var errInvalidConfig = errors.New("invalid GitHub CLI configuration")

func lockOwner(path string) (*ownerLock, error) {
	l, ok, err := tryOwner(path)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("GitHub CLI session is active")
	}
	return l, nil
}

func describeError(name string, err error) (string, string) {
	if boundary, ok := errors.AsType[*boundaryError](err); ok {
		return boundary.operation, boundary.kind
	}
	return name, errorKind(err)
}

type processResult struct {
	Code, Signal int
	ErrorType    string
}
