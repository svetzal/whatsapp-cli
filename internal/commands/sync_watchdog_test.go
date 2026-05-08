package commands

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.mau.fi/whatsmeow/types/events"
)

// newTestApp wires an App with mock dependencies for sync-loop tests.
func newTestApp(client WAClient) *App {
	return &App{
		client:  client,
		store:   &MockMessageStore{},
		version: "test",
	}
}

// drainSync calls Sync in a goroutine and returns a channel that closes
// once Sync returns. Caller is responsible for cancelling ctx.
func drainSync(app *App, ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = app.Sync(ctx)
	}()
	return done
}

// captureHandler returns a StartSyncFunc that delivers the registered event
// handler over a channel. Using a channel (instead of a shared variable plus
// Eventually) avoids data races under -race.
func captureHandler(handlerCh chan<- func(interface{})) func(ctx context.Context, h func(interface{})) error {
	return func(ctx context.Context, h func(interface{})) error {
		handlerCh <- h
		return nil
	}
}

func awaitHandler(t *testing.T, ch <-chan func(interface{})) func(interface{}) {
	t.Helper()
	select {
	case h := <-ch:
		return h
	case <-time.After(2 * time.Second):
		t.Fatal("event handler was never registered")
		return nil
	}
}

func TestSync_CancelsOnLoggedOutEvent(t *testing.T) {
	handlerCh := make(chan func(interface{}), 1)
	mock := &MockWAClient{StartSyncFunc: captureHandler(handlerCh)}
	app := newTestApp(mock)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := drainSync(app, ctx)
	handler := awaitHandler(t, handlerCh)

	handler(&events.LoggedOut{Reason: 401})

	select {
	case <-done:
		// Sync returned because the event handler cancelled the inner ctx.
	case <-time.After(2 * time.Second):
		t.Fatal("Sync did not return after LoggedOut event")
	}
}

func TestSync_CancelsOnStreamReplacedEvent(t *testing.T) {
	handlerCh := make(chan func(interface{}), 1)
	mock := &MockWAClient{StartSyncFunc: captureHandler(handlerCh)}
	app := newTestApp(mock)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := drainSync(app, ctx)
	handler := awaitHandler(t, handlerCh)

	handler(&events.StreamReplaced{})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Sync did not return after StreamReplaced event")
	}
}

func TestSync_WatchdogForcesReconnectAfterTimeout(t *testing.T) {
	// Shrink the watchdog windows so the test is fast.
	prevInterval, prevReset := syncWatchdogInterval, syncWatchdogResetAfter
	syncWatchdogInterval = 20 * time.Millisecond
	syncWatchdogResetAfter = 60 * time.Millisecond
	defer func() {
		syncWatchdogInterval = prevInterval
		syncWatchdogResetAfter = prevReset
	}()

	var disconnectCount, connectCount atomic.Int32
	connected := atomic.Bool{} // starts false → triggers watchdog

	mock := &MockWAClient{
		StartSyncFunc: func(ctx context.Context, h func(interface{})) error { return nil },
		IsConnectedFunc: func() bool {
			return connected.Load()
		},
		IsLoggedInFunc: func() bool {
			return connected.Load()
		},
		DisconnectFunc: func() {
			disconnectCount.Add(1)
		},
		ConnectFunc: func(ctx context.Context) error {
			connectCount.Add(1)
			// Mark connected so the watchdog sees recovery.
			connected.Store(true)
			return nil
		},
	}
	app := newTestApp(mock)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := drainSync(app, ctx)

	require.Eventually(t,
		func() bool { return disconnectCount.Load() >= 1 && connectCount.Load() >= 1 },
		1*time.Second, 10*time.Millisecond,
		"watchdog should have forced a reconnect cycle while disconnected",
	)

	cancel()
	<-done
}

func TestSync_WatchdogDoesNotInterveneWhenConnected(t *testing.T) {
	prevInterval, prevReset := syncWatchdogInterval, syncWatchdogResetAfter
	syncWatchdogInterval = 10 * time.Millisecond
	syncWatchdogResetAfter = 30 * time.Millisecond
	defer func() {
		syncWatchdogInterval = prevInterval
		syncWatchdogResetAfter = prevReset
	}()

	var disconnectCount, connectCount atomic.Int32

	mock := &MockWAClient{
		StartSyncFunc:   func(ctx context.Context, h func(interface{})) error { return nil },
		IsConnectedFunc: func() bool { return true },
		IsLoggedInFunc:  func() bool { return true },
		DisconnectFunc:  func() { disconnectCount.Add(1) },
		ConnectFunc:     func(ctx context.Context) error { connectCount.Add(1); return nil },
	}
	app := newTestApp(mock)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	<-drainSync(app, ctx)

	require.Equal(t, int32(0), disconnectCount.Load(), "watchdog should not Disconnect a healthy client")
	require.Equal(t, int32(0), connectCount.Load(), "watchdog should not Connect a healthy client")
}
