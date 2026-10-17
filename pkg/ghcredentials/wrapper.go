package ghcredentials

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const sessionEnv = "DEVSY_GH_SESSION"

// Run is the wrapper entry point. args starts with the wrapper's path, followed
// by untouched gh arguments. It bypasses Devsy's CLI/env/log initialization.
func Run(args []string) int {
	if len(args) == 0 {
		return 127
	}
	binary, err := realBinary(args[0])
	if err != nil {
		return 127
	}
	env := os.Environ()
	if active := os.Getenv(sessionEnv); active != "" && active == os.Getenv("GH_CONFIG_DIR") {
		return runGH(binary, args[1:], env, nil).Code
	}
	inv := &invocation{binary: binary, args: args[1:], env: env, diag: beginDiagnostic(binary)}
	defer inv.diag.finish()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	inv.recover(ctx)
	session := inv.prepare(ctx)
	if session == nil {
		return inv.diag.run(binary, inv.args, env, nil)
	}
	return inv.withSession(session)
}

type invocation struct {
	binary    string
	args, env []string
	diag      *diagnostic
}

func (inv *invocation) recover(ctx context.Context) {
	started := time.Now()
	root, err := sessionRoot()
	if err != nil {
		inv.diag.observe("session.recover", started, err)
		return
	}
	l, err := lock(ctx, filepath.Join(root, "sessions.lock"))
	if err != nil {
		inv.diag.observe("session.recover_lock", started, err)
		return
	}
	defer func() { _ = l.Close() }()
	inv.diag.Recovery = recoverSessions(ctx, root, inv.diag.path != "")
	inv.diag.observe("session.recover", started, nil)
}

func (inv *invocation) prepare(ctx context.Context) *session {
	started := time.Now()
	result, err := fetch(ctx)
	inv.diag.observe("relay.fetch", started, err)
	inv.diag.HostStatus = result.Status
	inv.diag.HostContext = result.Context
	inv.diag.hostResult(result)
	inv.diag.HostDecisions = result.Decisions
	if err != nil || len(result.Accounts) == 0 {
		inv.diag.Stage = "relay_unavailable"
		return nil
	}
	original, err := ConfigDir()
	if err != nil {
		inv.diag.Stage = "config_unavailable"
		return nil
	}
	inv.diag.ConfigDir = original
	started = time.Now()
	s, err := startSession(ctx, original, result.Accounts)
	inv.diag.observe("session.snapshot", started, err)
	if err != nil {
		inv.diag.Stage = "snapshot_failed"
		return nil
	}
	started = time.Now()
	if err := s.inject(ctx, inv.binary, inv.env); err != nil {
		inv.diag.observe("config.overlay", started, err)
		_ = os.RemoveAll(s.dir)
		_ = s.lock.Close()
		inv.diag.Stage = "overlay_failed"
		return nil
	}
	inv.diag.observe("config.overlay", started, nil)
	inv.diag.Hosts = s.decisions
	return s
}

func (inv *invocation) withSession(s *session) int {
	inv.diag.Stage = "overlay_ready"
	inv.diag.save(false)
	env := replaceEnv(inv.env, "GH_CONFIG_DIR", s.work())
	env = replaceEnv(env, sessionEnv, s.work())
	code := inv.diag.run(inv.binary, inv.args, env, s.lock.file)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	started := time.Now()
	if inv.diag.path != "" {
		s.writeback = &writebackSummary{}
	}
	err := s.finish(ctx)
	inv.diag.Writeback = s.writeback
	inv.diag.observe("config.writeback", started, err)
	if err != nil {
		inv.diag.Stage = "writeback_pending"
	} else {
		inv.diag.Stage = "written_back"
	}
	return code
}

func realBinary(wrapper string) (string, error) {
	self, _ := os.Stat(wrapper)
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		path := filepath.Join(dir, "gh")
		info, err := executableInfo(path)
		if err != nil {
			continue
		}
		if self != nil && os.SameFile(self, info) {
			continue
		}
		return filepath.Abs(path)
	}
	return "", exec.ErrNotFound
}

func replaceEnv(env []string, name, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, e := range env {
		if !strings.HasPrefix(e, name+"=") {
			out = append(out, e)
		}
	}
	return append(out, name+"="+value)
}

func fetch(ctx context.Context) (HostResult, error) {
	number, err := relayPort()
	if err != nil {
		return HostResult{}, err
	}
	// A loopback-only transport must not send tokens or relay requests through
	// a user-configured HTTP proxy, redirect, or an external endpoint.
	client := &http.Client{
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				dialer := net.Dialer{}
				return dialer.DialContext(
					ctx,
					"tcp",
					net.JoinHostPort("127.0.0.1", strconv.Itoa(number)),
				)
			},
		},
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		"http://localhost/github-credentials",
		nil,
	)
	if err != nil {
		return HostResult{}, boundary("relay.http", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return HostResult{}, boundary("relay.http", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return HostResult{}, &boundaryError{
			operation: "relay.http",
			kind:      "http_" + strconv.Itoa(resp.StatusCode),
		}
	}
	var result HostResult
	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result)
	if err != nil {
		return HostResult{}, &boundaryError{
			operation: "relay.decode",
			kind:      "schema_invalid",
			cause:     err,
		}
	}
	return result, nil
}

func executableInfo(path string) (os.FileInfo, error) {
	f, err := resolvedFile(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return nil, exec.ErrNotFound
	}
	return info, nil
}

func relayPort() (int, error) {
	port := os.Getenv("DEVSY_CREDENTIALS_SERVER_PORT")
	if port == "" {
		port = "12049"
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return 0, &boundaryError{operation: "relay.port", kind: "config_invalid"}
	}
	return number, nil
}
