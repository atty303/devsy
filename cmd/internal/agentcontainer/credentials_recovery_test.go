package agentcontainer

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/devsy-org/devsy/pkg/agent/tunnel"
	"github.com/devsy-org/devsy/pkg/credentials"
	"github.com/devsy-org/devsy/pkg/gitcredentials"
	"github.com/devsy-org/devsy/pkg/log"
	"github.com/devsy-org/devsy/pkg/stdio"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

const (
	recoveryContender = "bob"
	recoveryLogFormat = "json"
)

type recoverySession struct {
	host       *recoveryHost
	cancel     context.CancelFunc
	done       <-chan error
	disconnect func()
}

type recoveryHost struct {
	tunnel.UnimplementedTunnelServer
	pings atomic.Int32
}

type recoveryLog struct {
	mu   sync.Mutex
	text strings.Builder
}

func (l *recoveryLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.text.Write(p)
}

func (l *recoveryLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.text.String()
}

func (h *recoveryHost) Ping(context.Context, *tunnel.Empty) (*tunnel.Empty, error) {
	h.pings.Add(1)
	return &tunnel.Empty{}, nil
}

func (h *recoveryHost) GitUser(context.Context, *tunnel.Empty) (*tunnel.Message, error) {
	raw, _ := json.Marshal(gitcredentials.GitUser{
		Name: "Relay Test", Email: "relay@example.invalid",
	})
	return &tunnel.Message{Message: string(raw)}, nil
}

// Exercise the command's real stdio/RPC boundary without touching host Git state.
func startRecoveryCommand(t *testing.T, cmd *CredentialsServerCmd, port int) *recoverySession {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(dir, "git", "config"))
	t.Setenv("GIT_CONFIG_COUNT", "0")
	t.Setenv("GIT_CONFIG_PARAMETERS", "")
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "git"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "git", "config"), []byte(
		"[user]\nname = Relay Test\nemail = relay@example.invalid\n"), 0o600))
	remoteIn, hostOut, err := os.Pipe()
	require.NoError(t, err)
	hostIn, remoteOut, err := os.Pipe()
	require.NoError(t, err)
	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = remoteIn, remoteOut
	host := &recoveryHost{}
	server := grpc.NewServer()
	tunnel.RegisterTunnelServer(server, host)
	listener := stdio.NewStdioListener(hostIn, hostOut)
	go func() { _ = server.Serve(listener) }()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		done <- cmd.Run(ctx, port)
		close(exited)
	}()
	t.Cleanup(func() {
		cancel()
		_ = hostOut.Close()
		select {
		case <-exited:
		case <-time.After(3 * time.Second):
			t.Error("credentials command did not stop")
		}
		for _, f := range []*os.File{remoteIn, remoteOut, hostIn, hostOut} {
			_ = f.Close()
		}
		server.Stop()
		_ = listener.Close()
		os.Stdin, os.Stdout = oldIn, oldOut
	})
	return &recoverySession{
		host: host, cancel: cancel, done: done,
		disconnect: func() { _ = hostOut.Close() },
	}
}

func TestCredentialsRecoveryWaitsWithLiveRPC(t *testing.T) {
	port := startFakeCredentialsServer(t, "alice")
	session := startRecoveryCommand(t, &CredentialsServerCmd{User: recoveryContender}, port)
	select {
	case err := <-session.done:
		t.Fatalf("contender exited instead of waiting: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	require.Eventually(
		t,
		func() bool { return session.host.pings.Load() > 0 },
		time.Second,
		time.Millisecond,
	)
	owner, err := credentials.FetchOwner(t.Context(), port)
	require.NoError(t, err)
	require.Equal(t, "alice", owner)
	session.cancel()
	select {
	case err := <-session.done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("cancellation did not interrupt port wait")
	}
}

func TestCredentialsRecoveryDisconnectStopsWaiter(t *testing.T) {
	var sink recoveryLog
	log.Init(log.Config{Verbosity: 1, Format: recoveryLogFormat})
	remove := log.AddSink(&sink)
	defer remove()
	port := startFakeCredentialsServer(t, "alice")
	session := startRecoveryCommand(t, &CredentialsServerCmd{User: recoveryContender}, port)
	require.Eventually(
		t,
		func() bool { return session.host.pings.Load() > 0 },
		time.Second,
		time.Millisecond,
	)
	require.Eventually(t, func() bool {
		return strings.Contains(sink.String(), "waiting for credentials port")
	}, time.Second, time.Millisecond)
	session.disconnect()
	select {
	case err := <-session.done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("RPC EOF did not stop the port waiter")
	}
	require.Contains(t, sink.String(), "credentials services ended")
	require.Contains(t, sink.String(), "cause=EOF")
	require.NotContains(t, sink.String(), "credentials port acquired")
}

func TestCredentialsRecoveryKeepsPortUntilHelperCleanup(t *testing.T) {
	listener, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	ln := &credentialsListener{Listener: listener, cleanup: func() {
		close(entered)
		<-release
	}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- credentials.RunCredentialsServerWithListener(ctx, ln, &fakeCredentialsClient{}, "alice")
	}()
	require.Eventually(t, func() bool {
		_, fetchErr := credentials.FetchOwner(t.Context(), port)
		return fetchErr == nil
	}, time.Second, time.Millisecond)
	cancel()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("owner did not start helper cleanup")
	}
	contender, claimErr := claimPort(port)
	if contender != nil {
		_ = contender.Close()
	}
	require.ErrorIs(t, claimErr, errPortOwnedByAnotherSession)
	unblock()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("owner did not finish helper cleanup")
	}
	successor, err := claimPort(port)
	require.NoError(t, err)
	require.NoError(t, successor.Close())
}

func TestCredentialsRecoveryMultipleWaiters(t *testing.T) {
	ln, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		_ = credentials.RunCredentialsServerWithListener(ctx, ln, &fakeCredentialsClient{}, "alice")
	}()
	require.Eventually(t, func() bool {
		_, fetchErr := credentials.FetchOwner(t.Context(), port)
		return fetchErr == nil
	}, time.Second, time.Millisecond)
	results := make(chan recoveryClaim, 3)
	waitCtx, stopWaiters := context.WithCancel(t.Context())
	var waiters sync.WaitGroup
	t.Cleanup(func() {
		stopWaiters()
		waiters.Wait()
		close(results)
		for r := range results {
			r.close()
		}
	})
	for range 3 {
		waiters.Go(func() {
			listener, claimErr := (&CredentialsServerCmd{User: recoveryContender}).waitForPort(
				waitCtx,
				port,
			)
			results <- recoveryClaim{listener, claimErr}
		})
	}
	cancel()
	for range 3 {
		var acquired recoveryClaim
		select {
		case acquired = <-results:
		case <-time.After(4 * time.Second):
			t.Fatal("waiting contender did not acquire the released port")
		}
		require.NoError(t, acquired.err)
		t.Cleanup(func() { _ = acquired.ln.Close() })
		select {
		case other := <-results:
			require.NoError(t, other.err)
			require.NoError(t, other.ln.Close())
			t.Fatal("more than one waiting contender acquired the port")
		case <-time.After(50 * time.Millisecond):
		}
		require.NoError(t, acquired.ln.Close())
	}
}

func TestCredentialsRecoveryInvalidPortDoesNotWait(t *testing.T) {
	_, err := (&CredentialsServerCmd{}).waitForPort(t.Context(), -1)
	require.Error(t, err)
	require.NotErrorIs(t, err, errPortOwnedByAnotherSession)
}

type recoveryClaim struct {
	ln  net.Listener
	err error
}

func (r recoveryClaim) close() {
	if r.ln != nil {
		_ = r.ln.Close()
	}
}

func TestCredentialsRecoveryServerErrorCleansBeforeRelease(t *testing.T) {
	listener, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	port := listener.Addr().(*net.TCPAddr).Port
	failure := errors.New("test accept failure")
	var cleaned atomic.Bool
	var claimErr error
	ln := &credentialsListener{
		Listener: &recoveryFailedListener{Listener: listener, err: failure},
		cleanup: func() {
			contender, err := claimPort(port)
			claimErr = err
			if contender != nil {
				_ = contender.Close()
			}
			cleaned.Store(true)
		},
	}
	err = credentials.RunCredentialsServerWithListener(
		t.Context(), ln, &fakeCredentialsClient{}, "alice",
	)
	require.ErrorIs(t, err, failure)
	require.True(t, cleaned.Load())
	require.ErrorIs(t, claimErr, errPortOwnedByAnotherSession)
	successor, err := claimPort(port)
	require.NoError(t, err)
	require.NoError(t, successor.Close())
}

type recoveryFailedListener struct {
	net.Listener
	err error
}

func (l *recoveryFailedListener) Accept() (net.Conn, error) { return nil, l.err }

func TestCredentialsRecoveryHelperFailureKeepsServerUntilCancellation(t *testing.T) {
	const missingUser = "devsy-relay-missing-user"
	_, err := user.Lookup(missingUser)
	require.Error(t, err)
	listener, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())
	session := startRecoveryCommand(t, &CredentialsServerCmd{
		User: missingUser, ConfigureDockerHelper: true,
	}, port)
	require.Eventually(t, func() bool {
		owner, err := credentials.FetchOwner(context.Background(), port)
		return err == nil && owner == missingUser
	}, 3*time.Second, 10*time.Millisecond)
	contender, err := claimPort(port)
	if contender != nil {
		_ = contender.Close()
	}
	require.ErrorIs(t, err, errPortOwnedByAnotherSession)
	session.cancel()
	select {
	case err := <-session.done:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("canceled server did not exit")
	}
	successor, err := claimPort(port)
	require.NoError(t, err)
	require.NoError(t, successor.Close())
}
