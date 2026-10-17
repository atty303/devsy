package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVersionReservation(t *testing.T) {
	version, err := nextVersion(
		"a refs/tags/v0.0.0-relay.2\na refs/tags/v0.0.0-atty.9\na refs/tags/v0.0.0-atty.9^{}\na refs/tags/v1.24.0\n",
	)
	require.NoError(t, err)
	require.Equal(t, "v0.0.0-atty.10", version)
}

func TestConfigurationStopsUnknownChecksAndDuplicateSources(t *testing.T) {
	c := config{
		Upstream:       "devsy-org/devsy",
		UpstreamBranch: "main",
		Branches: []branch{
			{Repository: fork, Branch: "feature", Checks: []string{"new-check"}},
		},
	}
	require.ErrorContains(t, validate(c), "unknown check")
	c.Branches[0].Checks = nil
	require.ErrorContains(t, validate(c), "explicit checks array")
	c.Branches[0].Checks = []string{}
	require.NoError(t, validate(c))
	c.Branches[0].Checks = []string{"gh-relay"}
	c.Branches = append(c.Branches, c.Branches[0])
	require.ErrorContains(t, validate(c), "duplicate branch")
}

type mergeFixture struct {
	root, source, incoming, before, beforeTree string
	git                                        func(...string) string
}

func conflictFixture(t *testing.T) mergeFixture {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	require.NoError(t, os.Mkdir(source, 0o700))
	r := runner{}
	git := func(args ...string) string {
		t.Helper()
		value, err := r.git(source, args...)
		require.NoError(t, err)
		return value
	}
	git("init", "-b", "main")
	git("config", "user.name", "Fixture")
	git("config", "user.email", "fixture@example.invalid")
	key := filepath.Join(root, "signing-key")
	_, err := r.run(root, nil, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key)
	require.NoError(t, err)
	git("config", "gpg.format", "ssh")
	git("config", "user.signingkey", key)
	git("config", "commit.gpgsign", "true")
	write := func(text string) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(source, "file"), []byte(text), 0o600))
		git("add", "file")
	}
	write("base\n")
	git("commit", "-m", "base")
	git("checkout", "-b", "feature")
	write("feature\n")
	git("commit", "-m", "feature")
	incoming := git("rev-parse", "HEAD")
	git("checkout", "main")
	write("upstream\n")
	git("commit", "-m", "upstream")
	before := git("rev-parse", "HEAD")
	beforeTree := git("rev-parse", "HEAD^{tree}")
	return mergeFixture{root, source, incoming, before, beforeTree, git}
}

func TestConflictResolutionReplay(t *testing.T) {
	f := conflictFixture(t)
	root, source, incoming, before, beforeTree, git := f.root, f.source, f.incoming, f.before, f.beforeTree, f.git
	r := runner{}
	write := func(text string) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(source, "file"), []byte(text), 0o600))
		git("add", "file")
	}

	b := branch{Repository: fork, Branch: "feature", SHA: incoming}
	require.ErrorContains(t, mergeBranch(r, source, root, b), "merge conflict")
	git("merge", "--abort")
	// Record the complete resolved merge tree as a diff against the first parent.
	write("resolved\n")
	diff, err := r.run(source, nil, "git", "diff", "--cached", "--binary")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "resolution.patch"), diff, 0o600))
	git("reset", "--hard", before)
	b.Resolution = &resolution{Before: beforeTree, Incoming: incoming, Patch: "resolution.patch"}
	require.NoError(t, mergeBranch(r, source, root, b))
	require.Equal(t, "resolved", git("show", "HEAD:file"))
	require.Equal(t, before+" "+incoming, git("show", "-s", "--format=%P"))
	require.Contains(t, git("cat-file", "commit", "HEAD"), "gpgsig -----BEGIN SSH SIGNATURE-----")
	git("reset", "--hard", before)
	b.Resolution.Before = strings.Repeat("0", 40)
	require.ErrorContains(t, mergeBranch(r, source, root, b), "stale conflict resolution")
	git("merge", "--abort")
	git("reset", "--hard", before)
	b.Resolution.Before = beforeTree
	require.NoError(
		t,
		os.WriteFile(filepath.Join(root, "resolution.patch"), []byte("invalid patch"), 0o600),
	)
	require.Error(t, mergeBranch(r, source, root, b))
}

func TestRecordingDoesNotChangeCommandResult(t *testing.T) {
	var record bytes.Buffer
	with := runner{record: &record}
	without := runner{}
	a, err := with.run("", nil, "git", "--version")
	require.NoError(t, err)
	b, err := without.run("", nil, "git", "--version")
	require.NoError(t, err)
	require.Equal(t, a, b)
	require.Contains(t, record.String(), `"status":"ok"`)
	_, err = with.run("", nil, "git", "nonexistent-command")
	require.Error(t, err)
	require.Contains(t, record.String(), `"error_type":"process_exit"`)
}

func TestPatchCannotEscapeManagement(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "patch")
	require.NoError(t, os.WriteFile(outside, []byte("patch"), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "escape")))
	_, err := safePath(root, "escape")
	require.ErrorContains(t, err, "outside management")
}
