package opener

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBrowserReuseWaitsForPreparedHelperIdentity(t *testing.T) {
	setupTempHome(t)
	created, err := helperCreateTime(os.Getpid())
	require.NoError(t, err)
	expected := TunnelState{PID: os.Getpid(), CreateTime: created, PreparationVersion: 1}
	require.NoError(t, WriteTunnelState("ctx", "ws", expected))
	marker, err := browserPreparation("ctx", "ws", &expected)
	require.NoError(t, err)
	defer marker.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	unpreparedCtx, stopUnprepared := context.WithTimeout(ctx, 20*time.Millisecond)
	defer stopUnprepared()
	require.False(t, waitBrowserPreparation(unpreparedCtx, "ctx", "ws", &expected))
	require.NoError(t, marker.Mark())
	require.True(t, waitBrowserPreparation(ctx, "ctx", "ws", &expected))
	other := expected
	other.CreateTime++
	require.NoError(t, WriteTunnelState("ctx", "ws", other))
	require.False(t, waitBrowserPreparation(ctx, "ctx", "ws", &expected))
}

func TestBrowserReuseRejectsLegacyAndCanceledPreparation(t *testing.T) {
	setupTempHome(t)
	expected := TunnelState{PID: 123, CreateTime: 456}
	require.NoError(t, WriteTunnelState("ctx", "ws", expected))
	require.False(t, waitBrowserPreparation(context.Background(), "ctx", "ws", &expected))
	expected.PreparationVersion = 1
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.NoError(t, WriteTunnelState("ctx", "ws", expected))
	marker, err := browserPreparation("ctx", "ws", &expected)
	require.NoError(t, err)
	defer marker.Close()
	require.NoError(t, marker.Mark())
	require.False(t, waitBrowserPreparation(ctx, "ctx", "ws", &expected))
}
