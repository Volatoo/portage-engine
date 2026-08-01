package runtimecache

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// unreachableClient points at a closed port so every subscribe attempt fails
// immediately. MaxRetries is disabled to keep the retry timing the test's own.
func unreachableClient(t *testing.T) *Client {
	t.Helper()
	return &Client{
		redis: redis.NewClient(&redis.Options{
			Addr: "127.0.0.1:1", DialTimeout: 200 * time.Millisecond,
			MaxRetries: -1,
		}),
		prefix: "portage-engine-test",
		poolOK: true,
		health: Health{Enabled: true, OK: true},
	}
}

// TestSubscribeWakeSurvivesConnectionErrors pins the supervisor: a single
// connection error must not end the subscription for the life of the process.
func TestSubscribeWakeSurvivesConnectionErrors(t *testing.T) {
	client := unreachableClient(t)
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	err := client.SubscribeWake(ctx, func() {})
	if ctx.Err() == nil {
		t.Fatalf("SubscribeWake returned before its context ended: %v", err)
	}

	client.mu.RLock()
	health := client.health
	client.mu.RUnlock()
	if health.OK || !health.WakeSupervised || health.WakeSubscribed ||
		health.WakeError == "" {
		t.Fatalf("dead wake subscription reported as healthy: %+v", health)
	}
}

// TestPooledSuccessDoesNotMaskDeadWakeSubscription covers the reason the
// failure was invisible: presence and rate limiting keep recording successes on
// pooled commands after the subscription is gone.
func TestPooledSuccessDoesNotMaskDeadWakeSubscription(t *testing.T) {
	client := unreachableClient(t)
	defer func() { _ = client.Close() }()

	client.recordWake(false, context.DeadlineExceeded)
	client.record(nil)

	client.mu.RLock()
	health := client.health
	client.mu.RUnlock()
	if health.OK {
		t.Fatalf("pooled success washed a dead subscription green: %+v", health)
	}

	client.recordWake(true, nil)
	client.mu.RLock()
	health = client.health
	client.mu.RUnlock()
	if !health.OK || health.WakeError != "" {
		t.Fatalf("resubscribed client stayed unhealthy: %+v", health)
	}
}
