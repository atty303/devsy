package agentcontainer

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func checkoutGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git")
	cmd.Args = append(cmd.Args, "-C", dir)
	cmd.Args = append(cmd.Args, args...)
	cmd.Env = append(infoGitEnv(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	raw, err := cmd.CombinedOutput()
	require.NoError(t, err, string(raw))
	return strings.TrimSpace(string(raw))
}

func TestContainerInfoCheckout(t *testing.T) {
	const linkedBranch = "linked"
	base := t.TempDir()
	root := filepath.Join(base, "main checkout")
	require.NoError(t, os.Mkdir(root, 0o700))
	checkoutGit(t, root, "init", "-b", "main")
	checkoutGit(t, root, "-c", "user.name=Test", "-c", "user.email=test@example.invalid",
		"-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "initial")
	commit := checkoutGit(t, root, "rev-parse", "HEAD")
	worktree := filepath.Join(base, "linked checkout")
	checkoutGit(t, root, "worktree", "add", "-b", linkedBranch, worktree)
	subdir := filepath.Join(worktree, "nested")
	require.NoError(t, os.Mkdir(subdir, 0o700))
	detached := filepath.Join(base, "detached")
	checkoutGit(t, root, "worktree", "add", "--detach", detached)
	bare := filepath.Join(base, "bare.git")
	checkoutGit(t, root, "clone", "--bare", root, bare)
	unborn := filepath.Join(base, "unborn")
	require.NoError(t, os.Mkdir(unborn, 0o700))
	checkoutGit(t, unborn, "init", "-b", "new-branch")
	for _, tt := range []struct {
		name, dir, root, branch, commit string
	}{
		{"main", root, root, "main", commit},
		{"linked", worktree, worktree, linkedBranch, commit},
		{"subdirectory", subdir, worktree, linkedBranch, commit},
		{"detached", detached, detached, "", commit},
		{"outside", base, "", "", ""},
		{"bare repository", bare, "", "", ""},
		{"unborn", unborn, unborn, "new-branch", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Chdir(tt.dir)
			t.Setenv("GIT_DIR", filepath.Join(root, ".git"))
			t.Setenv("GIT_WORK_TREE", root)
			info := &containerInfo{}
			require.NoError(t, info.readCheckout(context.Background()))
			assertInfoPointer(t, info.CheckoutRoot, tt.root)
			assertInfoPointer(t, info.Branch, tt.branch)
			assertInfoPointer(t, info.Commit, tt.commit)
		})
	}
}

func assertInfoPointer(t *testing.T, got *string, want string) {
	t.Helper()
	if want == "" {
		require.Nil(t, got)
		return
	}
	require.NotNil(t, got)
	if filepath.IsAbs(want) {
		resolved, err := filepath.EvalSymlinks(want)
		require.NoError(t, err)
		want = resolved
	}
	require.Equal(t, want, *got)
}

func TestContainerInfoGitFailures(t *testing.T) {
	t.Run("missing Git", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		info := &containerInfo{}
		require.ErrorContains(
			t,
			info.readCheckout(context.Background()),
			"resolve Git checkout",
		)
	})
	t.Run("broken gitdir", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(
			t,
			os.WriteFile(
				filepath.Join(dir, ".git"),
				[]byte("gitdir: /nonexistent-devsy-test\n"),
				0o600,
			),
		)
		t.Chdir(dir)
		info := &containerInfo{}
		require.ErrorContains(
			t,
			info.readCheckout(context.Background()),
			"resolve Git checkout",
		)
	})
	t.Run("broken commit", func(t *testing.T) {
		dir := t.TempDir()
		checkoutGit(t, dir, "init", "-b", "main")
		ref := filepath.Join(dir, ".git", "refs", "heads", "main")
		require.NoError(t, os.WriteFile(ref, []byte(strings.Repeat("a", 40)+"\n"), 0o600))
		t.Chdir(dir)
		info := &containerInfo{}
		require.ErrorContains(t, info.readCheckout(context.Background()), "resolve Git commit")
	})
	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		info := &containerInfo{}
		require.ErrorContains(t, info.readCheckout(ctx), "context canceled")
	})
}

func TestContainerInfoUnreadableGitMetadata(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read mode 000 metadata")
	}
	parent := t.TempDir()
	checkoutGit(t, parent, "init", "-b", "parent")
	for _, dir := range []string{t.TempDir(), filepath.Join(parent, "nested")} {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			require.NoError(t, os.MkdirAll(dir, 0o700))
			checkoutGit(t, dir, "init", "-b", "main")
			root, err := os.OpenRoot(dir)
			require.NoError(t, err)
			t.Cleanup(func() { _ = root.Close() })
			metadata, err := root.Open(".git")
			require.NoError(t, err)
			t.Cleanup(func() { _ = metadata.Chmod(0o700); _ = metadata.Close() })
			require.NoError(t, metadata.Chmod(0))
			t.Chdir(dir)
			info := &containerInfo{}
			require.ErrorContains(t, info.readCheckout(context.Background()), "Git checkout")
		})
	}
}
