package tunnel

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestServicesRetrySuccessfulCollisionUntilCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	attempts := 0
	done := make(chan error, 1)
	go func() {
		done <- runServicesUntilCanceled(ctx, func() error {
			attempts++
			if attempts == 3 {
				cancel()
			}
			return nil
		})
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
		require.Equal(t, 3, attempts)
	case <-time.After(5 * time.Second):
		t.Fatal("successful collision must be retried while the connection remains open")
	}
}

func TestServicesCancelDuringRetryWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- runServicesUntilCanceled(ctx, func() error {
			close(started)
			return nil
		})
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(250 * time.Millisecond):
		t.Fatal("cancellation must interrupt the retry delay")
	}
}
