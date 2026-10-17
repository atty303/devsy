//go:build ghrelay

package ghcredentials_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/devsy-org/devsy/pkg/agent/tunnelserver"
	"github.com/stretchr/testify/require"
)

//go:embed testdata/Dockerfile testdata/fixture.py
var fixtureFiles embed.FS

// Run with task cli:test:gh-relay. All authentication is fictional, API traffic
// stays in a network-disabled container, and the host gh reads a disposable config.
func TestRealGitHubCLI(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	dir := t.TempDir()
	name := prepareFixture(t, ctx, dir)
	prepareHostAuthentication(t, dir)
	stop, done := startFixture(t, ctx, dir, name)
	defer stop()
	waitFixture(t, ctx, name, done)
	// stdout contains only boolean test results. Raw gh streams and credentials
	// stay inside the disposable fixture and are never attached to failures.
	//nolint:gosec // name is the generated test container.
	check := exec.CommandContext(
		ctx,
		"docker",
		"exec",
		name,
		"python3",
		"/fixture/fixture.py",
		"check",
	)
	report, err := check.CombinedOutput()
	require.NoError(t, err, "real gh checks: %s", report)
	verifyRealGHReport(t, report)

	verifyRelayFallback(t, ctx, name, stop)
}

func downloadGH(t *testing.T, ctx context.Context, dir string) {
	t.Helper()
	const version = "2.100.0"
	filename := "gh_" + version + "_linux_" + runtime.GOARCH + ".tar.gz"
	get := func(name string) []byte {
		req, err := http.NewRequestWithContext(
			ctx,
			http.MethodGet,
			"https://github.com/cli/cli/releases/download/v"+version+"/"+name,
			nil,
		)
		require.NoError(t, err)
		response, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = response.Body.Close() }()
		require.Equal(t, http.StatusOK, response.StatusCode)
		b, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))
		require.NoError(t, err)
		return b
	}
	archive := get(filename)
	sums := string(get("gh_" + version + "_checksums.txt"))
	hash := sha256.Sum256(archive)
	require.Contains(t, sums, fmt.Sprintf("%x  %s", hash, filename))
	z, err := gzip.NewReader(bytes.NewReader(archive))
	require.NoError(t, err)
	defer func() { _ = z.Close() }()
	tarReader := tar.NewReader(z)
	for {
		header, err := tarReader.Next()
		require.NoError(t, err)
		if strings.HasSuffix(header.Name, "/bin/gh") {
			binary, err := io.ReadAll(io.LimitReader(tarReader, 128<<20))
			require.NoError(t, err)
			// #nosec G306 -- Executable release binary, verified against its published checksum.
			require.NoError(t, os.WriteFile(filepath.Join(dir, "gh"), binary, 0o755))
			return
		}
	}
}

func verifyRealGHReport(t *testing.T, report []byte) {
	t.Helper()
	var results []struct {
		Check string `json:"check"`
		OK    bool   `json:"ok"`
	}
	require.NoError(t, json.Unmarshal(report, &results))
	for _, r := range results {
		require.True(t, r.OK, r.Check)
		t.Log(r.Check)
	}
}

func prepareFixture(t *testing.T, ctx context.Context, dir string) string {
	t.Helper()
	repo, err := filepath.Abs("../..")
	require.NoError(t, err)
	for _, name := range []string{"Dockerfile", "fixture.py"} {
		b, err := fixtureFiles.ReadFile("testdata/" + name)
		require.NoError(t, err)
		root, err := os.OpenRoot(dir)
		require.NoError(t, err)
		require.NoError(t, root.WriteFile(name, b, 0o600))
		require.NoError(t, root.Close())
	}
	random := make([]byte, 12)
	_, err = rand.Read(random)
	require.NoError(t, err)
	name := "devsy-gh-test-" + hex.EncodeToString(random)
	image := name + ":test"
	//nolint:gosec // image and directory are generated fixture resources.
	buildImage := exec.CommandContext(ctx, "docker", "build", "-q", "-t", image, dir)
	imageOut, err := buildImage.CombinedOutput()
	require.NoError(t, err, "fixture image: %s", imageOut)
	t.Cleanup(func() {
		//nolint:gosec // image is the random, test-owned tag.
		_ = exec.Command("docker", "image", "rm", image).Run()
	})
	//nolint:gosec // The output is inside t.TempDir; the source is this repository.
	build := exec.CommandContext(ctx, "go", "build", "-o", filepath.Join(dir, "devsy"), ".")
	build.Dir = repo
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+runtime.GOARCH, "CGO_ENABLED=0")
	out, err := build.CombinedOutput()
	require.NoError(t, err, "build: %s", out)
	downloadGH(t, ctx, dir)
	return name
}

func prepareHostAuthentication(t *testing.T, dir string) {
	t.Helper()
	type tokens struct {
		Forwarded string `json:"forwarded"`
		Local     string `json:"local"`
		Second    string `json:"second"`
		Invalid   string `json:"invalid"`
		Refresh   string `json:"refresh"`
	}
	accounts := map[string]tokens{}
	var hosts strings.Builder
	for _, host := range []string{"github.com", "enterprise.one.test", "enterprise.two.test"} {
		token := func() string {
			b := make([]byte, 20)
			_, err := rand.Read(b)
			require.NoError(t, err)
			return "ghp_" + hex.EncodeToString(b)
		}
		v := tokens{
			Forwarded: token(),
			Local:     token(),
			Second:    token(),
			Invalid:   token(),
			Refresh:   token(),
		}
		accounts[host] = v
		fmt.Fprintf(
			&hosts,
			"%s:\n    user: host-user\n    oauth_token: %s\n    users:\n        host-user:\n            oauth_token: %s\n",
			host,
			v.Forwarded,
			v.Forwarded,
		)
	}
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "host-gh"), 0o700))
	require.NoError(
		t,
		os.WriteFile(filepath.Join(dir, "host-gh", "hosts.yml"), []byte(hosts.String()), 0o600),
	)
	data, err := json.Marshal(accounts)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "accounts.json"), data, 0o600))
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "GH_HOST"} {
		t.Setenv(key, "")
	}
	t.Setenv("GH_CONFIG_DIR", filepath.Join(dir, "host-gh"))
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(dir, "no-gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

func startFixture(
	t *testing.T,
	ctx context.Context,
	dir, name string,
) (context.CancelFunc, <-chan error) {
	t.Helper()
	image := name + ":test"
	command := `cp /fixture/gh /usr/bin/gh
cp /fixture/devsy /usr/local/bin/devsy
mkdir -p /root/.local/share/keyrings
export PATH=/fixture:$PATH
exec dbus-run-session -- sh -c '
 printf "%s" "$DBUS_SESSION_BUS_ADDRESS" > /fixture/bus
 printf fixture-password | gnome-keyring-daemon --unlock >/dev/null 2>/tmp/keyring.log
 python3 /fixture/fixture.py server >/dev/null 2>/tmp/api.log &
 /usr/local/bin/devsy internal agent container credentials-server --user root --configure-github-helper
 sleep 600
'`
	//nolint:gosec // All mounts, commands, and image names belong to this disposable fixture.
	guest := exec.CommandContext(
		ctx,
		"docker",
		"run",
		"--rm",
		"--name",
		name,
		"--network",
		"none",
		"-i",
		"-v",
		dir+":/fixture",
		image,
		"sh",
		"-c",
		command,
	)
	reader, err := guest.StdoutPipe()
	require.NoError(t, err)
	writer, err := guest.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, guest.Start())
	t.Cleanup(func() {
		//nolint:gosec // name is the random, test-owned container name.
		_ = exec.Command("docker", "rm", "-f", name).Run()
		_ = guest.Wait()
	})
	tunnelCtx, tunnelCancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- tunnelserver.RunServicesServer(tunnelCtx, reader, writer, false, false, nil, nil,
			tunnelserver.WithAllowGitHubCredentials(true))
	}()

	return tunnelCancel, done
}

func waitFixture(t *testing.T, ctx context.Context, name string, done <-chan error) {
	t.Helper()
	ready := false
	for range 150 {
		//nolint:gosec // name is the generated test container.
		c := exec.CommandContext(
			ctx,
			"docker",
			"exec",
			name,
			"sh",
			"-c",
			"test -f /fixture/api-ready && test -f /usr/local/share/devsy/gh/bin/gh",
		)
		if c.Run() == nil {
			ready = true
			break
		}
		select {
		case err := <-done:
			t.Fatalf("relay ended before readiness: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
	}
	require.True(t, ready, "fixture was not ready")
}

func verifyRelayFallback(t *testing.T, ctx context.Context, name string, stop context.CancelFunc) {
	t.Helper()
	// A transport failure must execute normal gh once, without relayed auth.
	stop()
	//nolint:gosec // name is the test-owned container; only its relay process is stopped.
	_ = exec.CommandContext(ctx, "docker", "exec", name, "sh", "-c", "pkill -x devsy || true").
		Run()
	//nolint:gosec // name is the test-owned container, with an isolated configuration.
	fallback := exec.CommandContext(
		ctx,
		"docker",
		"exec",
		name,
		"bash",
		"-lc",
		"export XDG_CACHE_HOME=/fixture/guest-cache; "+
			"GH_CONFIG_DIR=/fixture/guest-gh gh config get editor && "+
			"GH_CONFIG_DIR=/fixture/fallback-gh gh config set editor nano && "+
			"GH_CONFIG_DIR=/fixture/fallback-gh gh config get editor",
	)
	plain, err := fallback.CombinedOutput()
	require.NoError(t, err, "fallback: %s", plain)
	require.Equal(t, "pending\nnano\n", string(plain))
}
