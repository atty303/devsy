package ghcredentials

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

func TestSessionMergePreservesDotfilesAndConcurrentChanges(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	t.Setenv("HOME", root)
	original := filepath.Join(root, "gh")
	target := filepath.Join(root, "dotfiles", configFile)
	writeTestFile(t, target, "editor: vi\npager: less\n")
	require.NoError(t, os.MkdirAll(original, 0o700))
	require.NoError(t, os.Symlink(target, filepath.Join(original, configFile)))
	s, err := startSession(context.Background(), original, nil)
	require.NoError(t, err)
	writeTestFile(t, filepath.Join(s.work(), configFile), "editor: nano\npager: less\n")
	s.writeback = &writebackSummary{}
	// Dotfiles reapplies a new original during the command.
	writeTestFile(t, target, "editor: emacs\npager: cat\nprompt: disabled\n")
	require.NoError(t, s.finish(context.Background()))
	link, err := os.Readlink(filepath.Join(original, configFile))
	require.NoError(t, err)
	require.Equal(t, target, link)
	got, err := readFile(target)
	require.NoError(t, err)
	n, err := decode(got)
	require.NoError(t, err)
	require.Equal(t, "nano", get(n, "editor").Value)
	require.Equal(t, "cat", get(n, "pager").Value)
	require.Equal(t, "disabled", get(n, "prompt").Value)
	require.Equal(t, 1, s.writeback.WrittenFiles)
	require.Equal(t, changeCount{Modified: 1, Conflicts: 1}, s.writeback.Changes["config.editor"])
	require.NotContains(t, s.writeback.Changes, "config.pager")
}

func TestSessionConcurrentInvocationsAndRecovery(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	t.Setenv("HOME", root)
	original := filepath.Join(root, "gh")
	writeTestFile(t, filepath.Join(original, configFile), "editor: vi\npager: less\n")
	a, err := startSession(context.Background(), original, nil)
	require.NoError(t, err)
	b, err := startSession(context.Background(), original, nil)
	require.NoError(t, err)
	writeTestFile(t, filepath.Join(a.work(), configFile), "editor: nano\npager: less\n")
	writeTestFile(t, filepath.Join(b.work(), configFile), "editor: vi\npager: cat\n")
	require.NoError(t, a.finish(context.Background()))
	// Simulate a killed wrapper releasing its ownership lock without cleanup.
	require.NoError(t, b.lock.Close())
	c, err := startSession(context.Background(), original, nil)
	require.NoError(t, err)
	got, err := os.ReadFile(filepath.Join(c.work(), configFile))
	require.NoError(t, err)
	n, err := decode(got)
	require.NoError(t, err)
	require.Equal(t, "nano", get(n, "editor").Value)
	require.Equal(t, "cat", get(n, "pager").Value)
	require.NoError(t, c.finish(context.Background()))
}

func TestStripForwardedCredentialsAndPreserveLocalLogin(t *testing.T) {
	a := Account{Host: "github.example", User: "relay-user", Token: "dummy-forwarded-value"}
	s := &session{Accounts: []Account{a}}
	for _, tc := range []struct {
		name, input    string
		local, keyring bool
	}{
		{"unchanged", `github.example:
  user: relay-user
  oauth_token: dummy-forwarded-value
  users:
    relay-user:
      oauth_token: dummy-forwarded-value
`, false, false},
		{"local login", `github.example:
  user: relay-user
  oauth_token: dummy-local-value
  users:
    relay-user:
      oauth_token: dummy-local-value
`, true, false},
		{"keyring login", "github.example:\n  user: relay-user\n  users:\n    relay-user: {}\n", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.strip([]byte(tc.input))
			require.NoError(t, err)
			// Do not include the produced configuration in assertion failures.
			n, err := decode(got)
			require.NoError(t, err)
			h := get(n, a.Host)
			require.Equal(t, tc.local, h != nil)
			if tc.local {
				require.NotNil(t, get(h, "user"))
			}
			if tc.keyring {
				require.Nil(t, get(h, "oauth_token"))
			}
		})
	}
}

func TestMergeAddedMappingsAndDeletions(t *testing.T) {
	got, err := mergeYAML(
		[]byte("aliases: {}\neditor: vi\n"),
		[]byte("aliases:\n  mine: repo view\n"),
		[]byte("aliases:\n  other: issue list\neditor: vi\npager: cat\n"),
	)
	require.NoError(t, err)
	n, err := decode(got)
	require.NoError(t, err)
	require.Nil(t, get(n, "editor"))
	require.Equal(t, "cat", get(n, "pager").Value)
	require.NotNil(t, get(get(n, "aliases"), "mine"))
	require.NotNil(t, get(get(n, "aliases"), "other"))
}

func TestDanglingDotfilesSymlinkKeepsPendingChanges(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	original, target := filepath.Join(root, "gh"), filepath.Join(root, "dotfiles.yml")
	writeTestFile(t, target, "editor: vi\n")
	require.NoError(t, os.MkdirAll(original, 0o700))
	link := filepath.Join(original, configFile)
	require.NoError(t, os.Symlink(target, link))
	s, err := startSession(context.Background(), original, nil)
	require.NoError(t, err)
	writeTestFile(t, filepath.Join(s.work(), configFile), "editor: nano\n")
	require.NoError(t, os.Remove(target))
	require.Error(t, s.finish(context.Background()))
	actual, err := os.Readlink(link)
	require.NoError(t, err)
	require.Equal(t, target, actual)
	writeTestFile(t, target, "editor: emacs\npager: cat\n")
	c, err := startSession(context.Background(), original, nil)
	require.NoError(t, err)
	data, err := readFile(filepath.Join(c.work(), configFile))
	require.NoError(t, err)
	n, err := decode(data)
	require.NoError(t, err)
	require.Equal(t, "nano", get(n, "editor").Value)
	require.Equal(t, "cat", get(n, "pager").Value)
	require.NoError(t, c.finish(context.Background()))
}

func TestDisabledWritebackDiagnosticsDoNotParseConfiguration(t *testing.T) {
	var summary *writebackSummary
	// Invalid YAML would fail diagnostic parsing if the disabled sink were entered.
	require.NoError(t, summary.inspect(configFile, []byte("["), []byte("["), []byte("[")))
}
