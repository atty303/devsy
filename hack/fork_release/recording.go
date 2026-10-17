package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

type boundedRecord struct {
	writer    io.Writer
	remaining int
	degraded  bool
}

func (b *boundedRecord) Write(p []byte) (int, error) {
	if len(p) > b.remaining {
		if !b.degraded {
			fmt.Fprintln(os.Stderr, "operation recording partial: capacity exceeded")
		}
		b.degraded = true
		return len(p), nil
	}
	b.remaining -= len(p)
	n, err := b.writer.Write(p)
	if err != nil && !b.degraded {
		fmt.Fprintln(os.Stderr, "operation recording partial: write unavailable")
		b.degraded = true
	}
	return n, err
}

func recordedDispatch(a assembly, command string, values []string) error {
	recordStatus(a.r.record, command, "started")
	err := dispatch(a, command, values)
	status := "complete"
	if err != nil {
		status = "failed"
	}
	a.r.observe(operation{Name: command, Status: status, ErrorType: failureType(err)})
	return err
}

func recordStatus(w io.Writer, command, status string) {
	if w != nil {
		_ = json.NewEncoder(w).Encode(operation{
			Time: time.Now().UTC().Format(time.RFC3339Nano), Name: command, Status: status,
		})
	}
}

func failureType(err error) string {
	if err == nil {
		return ""
	}
	for _, item := range []struct {
		cause error
		kind  string
	}{
		{errConfigInvalid, "config_invalid"},
		{errRetired, "retired"},
		{errSourceMismatch, "source_mismatch"},
		{errProcess, "process_exit"},
		{os.ErrPermission, "permission_denied"},
		{os.ErrNotExist, "input_unavailable"},
	} {
		if errors.Is(err, item.cause) {
			return item.kind
		}
	}
	return "operation_failed"
}

func (r runner) observe(event operation) {
	if r.record != nil {
		event.Time = time.Now().UTC().Format(time.RFC3339Nano)
		_ = json.NewEncoder(r.record).Encode(event)
	}
}
