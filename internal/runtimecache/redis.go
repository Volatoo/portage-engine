// Package runtimecache provides Redis-backed ephemeral coordination. No build
// correctness decision is stored here; PostgreSQL leases and fences remain the
// authority if Redis is unavailable or loses data.
package runtimecache

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/slchris/portage-engine/pkg/config"
)

// Health reports Redis availability and live control-plane presence.
type Health struct {
	Enabled       bool      `json:"enabled"`
	OK            bool      `json:"ok"`
	LastError     string    `json:"last_error,omitempty"`
	LastSuccessAt time.Time `json:"last_success_at,omitempty"`
	Presence      int64     `json:"control_plane_presence"`
}

// Client provides optional Redis-backed acceleration without owning correctness state.
type Client struct {
	redis  *redis.Client
	prefix string
	mu     sync.RWMutex
	health Health
}

// Open connects to Redis and verifies the configured endpoint.
func Open(ctx context.Context, cfg config.CacheConfig) (*Client, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	options := &redis.Options{
		Addr:     net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
		Password: cfg.Password, DB: cfg.DB,
		DialTimeout: 3 * time.Second, ReadTimeout: 3 * time.Second,
		WriteTimeout: 3 * time.Second, PoolTimeout: 4 * time.Second,
		MaxRetries: 2,
	}
	if cfg.TLSEnabled {
		options.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, ServerName: cfg.Host}
	}
	client := &Client{
		redis:  redis.NewClient(options),
		prefix: strings.Trim(strings.TrimSpace(cfg.KeyPrefix), ":"),
		health: Health{Enabled: true},
	}
	if client.prefix == "" {
		client.prefix = "portage-engine"
	}
	if err := client.redis.Ping(ctx).Err(); err != nil {
		_ = client.redis.Close()
		return nil, fmt.Errorf("ping Redis: %w", err)
	}
	client.record(nil)
	return client, nil
}

func (c *Client) key(parts ...string) string {
	return c.prefix + ":" + strings.Join(parts, ":")
}

func (c *Client) record(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.health.Enabled = true
	if err != nil {
		c.health.OK = false
		c.health.LastError = err.Error()
		return
	}
	c.health.OK = true
	c.health.LastError = ""
	c.health.LastSuccessAt = time.Now().UTC()
}

// Health probes Redis and returns the latest cache health snapshot.
func (c *Client) Health(ctx context.Context) Health {
	if c == nil {
		return Health{}
	}
	err := c.redis.Ping(ctx).Err()
	c.record(err)
	c.mu.RLock()
	result := c.health
	c.mu.RUnlock()
	if err == nil {
		cutoff := time.Now().Add(-45 * time.Second).UnixMilli()
		key := c.key("presence", "control-plane")
		pipe := c.redis.Pipeline()
		pipe.ZRemRangeByScore(ctx, key, "-inf", strconv.FormatInt(cutoff, 10))
		count := pipe.ZCard(ctx, key)
		if _, pipeErr := pipe.Exec(ctx); pipeErr == nil {
			result.Presence = count.Val()
		}
	}
	return result
}

// TouchPresence refreshes one control-plane replica's ephemeral presence.
func (c *Client) TouchPresence(ctx context.Context, instance string, ttl time.Duration) error {
	if c == nil {
		return nil
	}
	key := c.key("presence", "control-plane")
	now := time.Now()
	pipe := c.redis.Pipeline()
	pipe.ZAdd(ctx, key, redis.Z{Score: float64(now.UnixMilli()), Member: instance})
	pipe.ZRemRangeByScore(ctx, key, "-inf", strconv.FormatInt(now.Add(-ttl).UnixMilli(), 10))
	pipe.Expire(ctx, key, ttl*2)
	_, err := pipe.Exec(ctx)
	c.record(err)
	return err
}

// PublishWake notifies scheduler replicas that durable work may be available.
func (c *Client) PublishWake(ctx context.Context) error {
	if c == nil {
		return nil
	}
	err := c.redis.Publish(ctx, c.key("scheduler", "wake"), "1").Err()
	c.record(err)
	return err
}

// SubscribeWake invokes wake for ephemeral scheduler notifications.
func (c *Client) SubscribeWake(ctx context.Context, wake func()) error {
	return c.subscribe(ctx, c.key("scheduler", "wake"), func(string) { wake() })
}

// JobEvent is the low-cardinality payload broadcast after a durable job update.
type JobEvent struct {
	JobID     string    `json:"job_id"`
	Status    string    `json:"status"`
	UpdatedAt time.Time `json:"updated_at"`
}

// PublishJobEvent broadcasts a durable job projection update.
func (c *Client) PublishJobEvent(ctx context.Context, event JobEvent) error {
	if c == nil {
		return nil
	}
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	err = c.redis.Publish(ctx, c.key("events", "jobs"), data).Err()
	c.record(err)
	return err
}

// StreamJobEvents streams ephemeral job notifications to receive.
func (c *Client) StreamJobEvents(ctx context.Context, receive func(string) error) error {
	return c.subscribeErr(ctx, c.key("events", "jobs"), receive)
}

func (c *Client) subscribe(ctx context.Context, channel string, receive func(string)) error {
	return c.subscribeErr(ctx, channel, func(payload string) error {
		receive(payload)
		return nil
	})
}

func (c *Client) subscribeErr(ctx context.Context, channel string, receive func(string) error) error {
	if c == nil {
		return fmt.Errorf("redis is disabled")
	}
	pubsub := c.redis.Subscribe(ctx, channel)
	defer func() { _ = pubsub.Close() }()
	if _, err := pubsub.Receive(ctx); err != nil {
		c.record(err)
		return err
	}
	for {
		message, err := pubsub.ReceiveMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			c.record(err)
			return err
		}
		c.record(nil)
		if err := receive(message.Payload); err != nil {
			return err
		}
	}
}

var tokenBucket = redis.NewScript(`
local rate = tonumber(ARGV[1])
local capacity = tonumber(ARGV[2])
local now_parts = redis.call('TIME')
local now = tonumber(now_parts[1]) + tonumber(now_parts[2]) / 1000000
local values = redis.call('HMGET', KEYS[1], 'tokens', 'updated')
local tokens = tonumber(values[1])
local updated = tonumber(values[2])
if tokens == nil then tokens = capacity end
if updated == nil then updated = now end
tokens = math.min(capacity, tokens + math.max(0, now - updated) * rate)
local allowed = 0
if tokens >= 1 then
  tokens = tokens - 1
  allowed = 1
end
redis.call('HSET', KEYS[1], 'tokens', tokens, 'updated', now)
redis.call('EXPIRE', KEYS[1], math.max(60, math.ceil(capacity / rate) * 2))
return allowed
`)

// Allow applies the optional Redis token-bucket rate limit.
func (c *Client) Allow(ctx context.Context, identity string, perMinute, burst int) (bool, error) {
	if c == nil || perMinute <= 0 || burst <= 0 {
		return true, nil
	}
	identity = strings.NewReplacer(":", "_", "/", "_", " ", "_").Replace(identity)
	result, err := tokenBucket.Run(ctx, c.redis, []string{c.key("ratelimit", identity)},
		float64(perMinute)/60.0, burst).Int()
	c.record(err)
	if err != nil {
		return true, err // fail open: Redis is never a correctness dependency
	}
	return result == 1, nil
}

// Close releases the Redis client connection pool.
func (c *Client) Close() error {
	if c == nil {
		return nil
	}
	return c.redis.Close()
}
