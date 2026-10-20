package tunnel

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/devsy-org/devsy/pkg/credentials"
	"github.com/stretchr/testify/require"
)

func TestBrowserAutoOpenWaitsForInstallationAndServing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	install, serving := make(chan struct{}), make(chan struct{})
	probeStarted, installed := make(chan struct{}), make(chan struct{})
	opened, exited := make(chan struct{}), make(chan error, 1)
	serviceStopped := make(chan struct{})
	runner := servicePreparation{
		run: func() error { defer close(serviceStopped); <-ctx.Done(); return nil },
		probe: func(ctx context.Context) credentials.Readiness {
			close(probeStarted)
			if !waitPreparationSignal(ctx, install) {
				return credentials.Readiness{GitHubState: credentialsUnavailable}
			}
			close(installed)
			if !waitPreparationSignal(ctx, serving) {
				return credentials.Readiness{GitHubState: credentialsUnavailable}
			}
			return credentials.Readiness{GitHubState: installedPreparation}
		},
	}
	go func() {
		exited <- runPreparedBrowserServices(ctx, BrowserTunnelParams{
			OnPrepared: func(context.Context) { close(opened) },
		}, func() credentials.Readiness { return runner.start(ctx, true) })
	}()
	require.True(t, waitPreparationSignal(ctx, probeStarted))
	assertPreparationPending(t, opened)
	close(install)
	require.True(t, waitPreparationSignal(ctx, installed))
	assertPreparationPending(t, opened)
	close(serving)
	require.True(t, waitPreparationSignal(ctx, opened))
	assertPreparationPending(t, exited)
	cancel()
	require.True(t, waitPreparationSignal(context.Background(), serviceStopped))
	select {
	case err := <-exited:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("browser service lifetime did not stop")
	}
}

func TestDaemonBrowserAutoOpenReceivesPreparationAndKeepsLifetime(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	started, ready, opened := make(chan struct{}), make(chan struct{}), make(chan struct{})
	exited := make(chan error, 1)
	go func() {
		exited <- StartBrowserTunnel(ctx, BrowserTunnelParams{
			DaemonStartFunc: func(ctx context.Context, prepared func()) error {
				close(started)
				if !waitPreparationSignal(ctx, ready) {
					return ctx.Err()
				}
				prepared()
				<-ctx.Done()
				return nil
			},
			OnPrepared: func(context.Context) { close(opened) },
		})
	}()
	require.True(t, waitPreparationSignal(ctx, started))
	assertPreparationPending(t, opened)
	close(ready)
	require.True(t, waitPreparationSignal(ctx, opened))
	assertPreparationPending(t, exited)
	cancel()
	select {
	case err := <-exited:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("daemon browser lifetime did not stop")
	}
}

func TestBrowserAutoOpenDoesNotRunAfterFailureOrCancellation(t *testing.T) {
	opened := false
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := BrowserTunnelParams{OnPrepared: func(context.Context) { opened = true }}
	require.NoError(t, runPreparedBrowserServices(ctx, p, func() credentials.Readiness {
		return credentials.Readiness{GitHubState: credentialsUnavailable}
	}))
	require.False(t, opened)
	want := errors.New("daemon transport unavailable")
	p.DaemonStartFunc = func(context.Context, func()) error { return want }
	require.ErrorIs(t, StartBrowserTunnel(context.Background(), p), want)
	require.False(t, opened)
}
