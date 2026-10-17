package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPrepareRecordsDistinctEarlyStops(t *testing.T) {
	for _, tc := range []struct {
		name, kind string
		branches   []branch
	}{
		{name: "retired", kind: "retired"},
		{name: "invalid", kind: "config_invalid", branches: []branch{
			{Repository: fork, Branch: "unregistered-feature", Checks: []string{"unknown"}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(manager, "hack/fork_release"), 0o700))
			c := config{Upstream: "devsy-org/devsy", UpstreamBranch: "main", Branches: tc.branches}
			require.NoError(
				t,
				writeJSON(filepath.Join(manager, "hack/fork_release/config.json"), c),
			)
			out := filepath.Join(t.TempDir(), "assembly")
			require.Error(t, execute([]string{"prepare", "--root", manager, "--out", out}))
			data, err := os.ReadFile(filepath.Join(out, "operations.jsonl"))
			require.NoError(t, err)
			require.Contains(t, string(data), `"error_type":"`+tc.kind+`"`)
			t.Setenv("DEVSY_FORK_RECORDING", "0")
			disabled := filepath.Join(t.TempDir(), "assembly")
			require.Error(t, execute([]string{"prepare", "--root", manager, "--out", disabled}))
			_, err = os.Stat(filepath.Join(disabled, "operations.jsonl"))
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

type failedRecord struct{}

func (failedRecord) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestRecordingFailureAndCapacityPreserveCommandResult(t *testing.T) {
	var buffer bytes.Buffer
	for _, record := range []io.Writer{failedRecord{}, &boundedRecord{writer: &buffer, remaining: 0}} {
		output, err := (runner{record: record}).run("", nil, "git", "--version")
		require.NoError(t, err)
		require.Contains(t, string(output), "git version")
	}
	require.Empty(t, buffer.String())
}

func TestRecordingCannotClaimCompleteAfterDroppingAnOperation(t *testing.T) {
	var buffer bytes.Buffer
	record := boundedRecord{writer: &buffer, remaining: 5}
	_, err := record.Write([]byte("too large"))
	require.NoError(t, err)
	_, err = record.Write([]byte("done"))
	require.NoError(t, err)
	require.True(t, record.degraded)
	require.Empty(t, buffer.String())
}
