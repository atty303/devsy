package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"al.essio.dev/pkg/shellescape"
	"github.com/devsy-org/devsy/pkg/config"
	"github.com/stretchr/testify/require"
)

const (
	infoContainerCommand = "container"
	infoCommand          = "info"
	infoWorkspaceID      = "test-workspace"
	infoIncompleteResult = "incomplete or failed"
	infoSetupFixture     = `{
 "MergedConfig":{"remoteUser":"configured-user","containerUser":"other-user","remoteEnv":{"TOKEN":"do-not-output"}},
 "ContainerDetails":{"Config":{"User":"image-user"}}
 }`
)

func containerInfoFixture(t *testing.T, result string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("container commands require sh")
	}
	dir := t.TempDir()
	fixture := filepath.Join(dir, "setup-result.json")
	require.NoError(t, os.WriteFile(fixture, []byte(result), 0o600))
	script := "#!/bin/sh\nexec /bin/cat " + shellescape.Quote(fixture) + "\n"
	file, err := os.CreateTemp(dir, "sh-*")
	require.NoError(t, err)
	_, err = file.WriteString(script)
	require.NoError(t, err)
	require.NoError(t, file.Chmod(0o700))
	require.NoError(t, file.Close())
	require.NoError(t, os.Symlink(file.Name(), filepath.Join(dir, "sh")))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(config.EnvWorkspaceID, infoWorkspaceID)
	t.Setenv("DEVSY_UI", "")
	t.Setenv("LC_ALL", "C")
	t.Setenv("USER", "wrong-os-user")
	t.Setenv("LOGNAME", "wrong-os-user")
	return fixture
}

func executeContainerInfo(t *testing.T, path []string, format string) (string, error) {
	t.Helper()
	root, _ := BuildRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	args := append([]string{}, path...)
	args = append(
		args,
		"--home",
		t.TempDir(),
		"--context",
		"not-configured",
		"--provider",
		"not-configured",
		"--result-format",
		format,
	)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestContainerInfoEntrypoints(t *testing.T) {
	containerInfoFixture(t, infoSetupFixture)
	t.Chdir(t.TempDir())
	for _, path := range containerInfoEntrypoints() {
		t.Run(path[0], func(t *testing.T) {
			raw, err := executeContainerInfo(t, path, "json")
			require.NoError(t, err)
			var got map[string]any
			require.NoError(t, json.Unmarshal([]byte(raw), &got))
			require.Equal(t, map[string]any{
				"sshCommand": "ssh configured-user@test-workspace.devsy",
				"sshHost":    "test-workspace.devsy", "remoteUser": "configured-user",
				"checkoutRoot": nil, "branch": nil, "commit": nil,
			}, got)
			text, err := executeContainerInfo(t, path, "plain")
			require.NoError(t, err)
			for _, value := range []string{
				"Field", "Value", "SSH command", "SSH host", "Remote user", "Checkout root", "Branch", "Commit",
				"ssh configured-user@test-workspace.devsy", "(none)",
			} {
				require.Contains(t, text, value)
			}
		})
	}
}

func TestContainerInfoSetupFailures(t *testing.T) {
	for _, tt := range []struct{ name, result, workspaceID, want string }{
		{"missing workspace", infoSetupFixture, "", config.EnvWorkspaceID},
		{"invalid JSON", "{", infoWorkspaceID, "parse container setup result"},
		{"null result", "null", infoWorkspaceID, infoIncompleteResult},
		{"missing config", `{"ContainerDetails":{}}`, infoWorkspaceID, infoIncompleteResult},
		{"missing container", `{"MergedConfig":{}}`, infoWorkspaceID, infoIncompleteResult},
		{
			"failed setup", `{"MergedConfig":{},"ContainerDetails":{},"Error":"private-error"}`,
			infoWorkspaceID, infoIncompleteResult,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			containerInfoFixture(t, tt.result)
			t.Setenv(config.EnvWorkspaceID, tt.workspaceID)
			for _, path := range containerInfoEntrypoints() {
				raw, err := executeContainerInfo(t, path, "json")
				require.ErrorContains(t, err, tt.want)
				require.Empty(t, raw)
			}
		})
	}
}

func TestContainerInfoReadFailure(t *testing.T) {
	fixture := containerInfoFixture(t, infoSetupFixture)
	require.NoError(t, os.Remove(fixture))
	raw, err := executeContainerInfo(t, []string{infoContainerCommand, infoCommand}, "json")
	require.ErrorContains(t, err, "read selected container setup result")
	require.Empty(t, raw)
}

func containerInfoEntrypoints() [][]string {
	return [][]string{
		{infoContainerCommand, infoCommand},
		{"internal", "agent", infoContainerCommand, infoCommand},
	}
}

func TestContainerInfoFailureRendering(t *testing.T) {
	fixture := containerInfoFixture(t, infoSetupFixture)
	require.NoError(t, os.Remove(fixture))
	_, err := executeContainerInfo(t, containerInfoEntrypoints()[0], "json")
	require.Error(t, err)
	for _, machine := range []bool{false, true} {
		reader, writer, pipeErr := os.Pipe()
		require.NoError(t, pipeErr)
		original := os.Stderr
		os.Stderr = writer
		code := exitCodeForError(err, machine)
		os.Stderr = original
		require.NoError(t, writer.Close())
		raw, readErr := io.ReadAll(reader)
		require.NoError(t, readErr)
		require.NoError(t, reader.Close())
		require.Equal(t, 1, code)
		require.Contains(t, string(raw), "read selected container setup result")
		require.Contains(t, string(raw), "No such file or directory")
	}
}
