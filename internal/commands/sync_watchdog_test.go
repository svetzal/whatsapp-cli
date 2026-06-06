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

func TestSync_ReclaimsStreamOnStreamReplacedEvent(t *testing.T) {
	// A single StreamReplaced should reclaim (Disconnect+Connect), not exit.
	prevBackoff := streamReclaimBackoff
	streamReclaimBackoff = 10 * time.Millisecond
	defer func() { streamReclaimBackoff = prevBackoff }()

	var disconnectCount, connectCount atomic.Int32
	handlerCh := make(chan func(interface{}), 1)
	mock := &MockWAClient{
		StartSyncFunc:  captureHandler(handlerCh),
		DisconnectFunc: func() { disconnectCount.Add(1) },
		ConnectFunc:    func(ctx context.Context) error { connectCount.Add(1); return nil },
	}
	app := newTestApp(mock)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := drainSync(app, ctx)
	handler := awaitHandler(t, handlerCh)

	handler(&events.StreamReplaced{})

	require.Eventually(t,
		func() bool { return disconnectCount.Load() >= 1 && connectCount.Load() >= 1 },
		2*time.Second, 10*time.Millisecond,
		"reclaimer should have reconnected after a single StreamReplaced",
	)

	// Sync must still be running — a single replacement is not fatal.
	select {
	case <-done:
		t.Fatal("Sync returned on a single StreamReplaced; it should reclaim, not exit")
	case <-time.After(50 * time.Millisecond):
	}

	cancel()
	<-done
}

func TestSync_GivesUpAfterRepeatedStreamReplaced(t *testing.T) {
	// A persistent competitor (rapid repeated StreamReplaced) should make the
	// reclaimer give up and let Sync return rather than ping-pong forever.
	prevBackoff, prevWindow, prevMax := streamReclaimBackoff, streamReclaimWindow, streamReclaimMaxAttempts
	streamReclaimBackoff = 5 * time.Millisecond
	streamReclaimWindow = 5 * time.Second
	streamReclaimMaxAttempts = 3
	defer func() {
		streamReclaimBackoff = prevBackoff
		streamReclaimWindow = prevWindow
		streamReclaimMaxAttempts = prevMax
	}()

	var connectCount atomic.Int32
	handlerCh := make(chan func(interface{}), 1)
	mock := &MockWAClient{
		StartSyncFunc:  captureHandler(handlerCh),
		DisconnectFunc: func() {},
		ConnectFunc:    func(ctx context.Context) error { connectCount.Add(1); return nil },
	}
	app := newTestApp(mock)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := drainSync(app, ctx)
	handler := awaitHandler(t, handlerCh)

	// Fire more replacements than the cap. Each reclaim attempt re-arms the
	// coalesced channel only after it finishes, so drive them from a goroutine
	// that keeps the signal pending until the reclaimer gives up. Capture the
	// cap into a local and stop once Sync returns so this goroutine neither
	// reads the tunable global nor outlives the test (both would race the
	// deferred restore above).
	maxAttempts := streamReclaimMaxAttempts
	fires := maxAttempts + 2
	stop := make(chan struct{})
	go func() {
		for i := 0; i < fires; i++ {
			handler(&events.StreamReplaced{})
			select {
			case <-stop:
				return
			case <-time.After(15 * time.Millisecond):
			}
		}
	}()

	select {
	case <-done:
		// Reclaimer hit the cap and cancelled the sync.
	case <-time.After(3 * time.Second):
		close(stop)
		t.Fatal("Sync did not return after repeated StreamReplaced events")
	}
	close(stop)

	require.LessOrEqual(t, connectCount.Load(), int32(maxAttempts),
		"reclaimer should not reconnect more than the attempt cap before giving up")
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
