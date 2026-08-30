// Package redisclient provides the minimal key/value operation set the
// Redis-backed SessionStore (internal/session) and DeviceRegistry
// (internal/devices) implementations need, isolating the go-redis
// dependency to this one package. Both consumers depend on the KVStore
// interface rather than *redis.Client directly, so they can be tested
// against a simple in-memory fake without a live Redis server.
package redisclient

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrKeyNotFound is returned by Get when key does not exist.
var ErrKeyNotFound = errors.New("redisclient: key not found")

// KVStore is the Redis operation subset consumers need: string get/set/del
// with TTL, and pattern-based enumeration for a low-cardinality keyspace
// (paired devices, pairing codes, active sessions -- tens to low
// thousands of keys, not millions, so a KEYS-based scan is acceptable
// here rather than requiring cursor-based SCAN handling in every caller).
type KVStore interface {
	// Get returns the value for key, or ErrKeyNotFound if it does not
	// exist.
	Get(ctx context.Context, key string) (string, error)
	// Set stores value under key. ttl <= 0 means no expiry.
	Set(ctx context.Context, key, value string, ttl time.Duration) error
	// Expire updates key's TTL without rewriting its value. A no-op
	// (returns nil) if key does not exist -- callers that need to know
	// whether the key existed should Get first.
	Expire(ctx context.Context, key string, ttl time.Duration) error
	// Del deletes the given keys. Deleting an absent key is not an error.
	Del(ctx context.Context, keys ...string) error
	// Keys returns every key matching pattern (a glob, e.g. "device:*").
	Keys(ctx context.Context, pattern string) ([]string, error)
	// Ping verifies connectivity (used at startup to fail fast rather
	// than the first request discovering Redis is unreachable).
	Ping(ctx context.Context) error
	// Close releases the underlying connection pool.
	Close() error
}

// client adapts *redis.Client to KVStore.
type client struct {
	rdb *redis.Client
}

// New connects to a Redis server at addr (host:port) and returns a
// KVStore backed by it. It does not verify connectivity itself -- call
// Ping to fail fast at startup.
func New(addr string) KVStore {
	return &client{rdb: redis.NewClient(&redis.Options{Addr: addr})}
}

func (c *client) Get(ctx context.Context, key string) (string, error) {
	v, err := c.rdb.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", ErrKeyNotFound
	}
	return v, err
}

func (c *client) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	if ttl < 0 {
		ttl = 0
	}
	return c.rdb.Set(ctx, key, value, ttl).Err()
}

func (c *client) Expire(ctx context.Context, key string, ttl time.Duration) error {
	if ttl < 0 {
		ttl = 0
	}
	return c.rdb.Expire(ctx, key, ttl).Err()
}

func (c *client) Del(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	return c.rdb.Del(ctx, keys...).Err()
}

func (c *client) Keys(ctx context.Context, pattern string) ([]string, error) {
	return c.rdb.Keys(ctx, pattern).Result()
}

func (c *client) Ping(ctx context.Context) error {
	return c.rdb.Ping(ctx).Err()
}

func (c *client) Close() error {
	return c.rdb.Close()
}

// PubSub is the publish/subscribe operation set the cross-replica dispatch
// bridge (internal/agent) needs (docs/specs/backend.md Section 10, Section
// 19: "multi-replica"). A concrete KVStore returned by New also implements
// PubSub; callers type-assert (kv.(PubSub)) rather than this package
// exposing two separate constructors, since in practice both operate over
// the same underlying connection to the same Redis instance.
type PubSub interface {
	// Publish sends payload to every current subscriber of channel.
	Publish(ctx context.Context, channel string, payload []byte) error
	// Subscribe returns a Subscription delivering every message published
	// to channel from this point on. The caller must Close it when done.
	Subscribe(ctx context.Context, channel string) (Subscription, error)
}

// Subscription is one active channel subscription.
type Subscription interface {
	// Messages delivers each published payload in order. The channel is
	// closed when the subscription is closed or the connection is lost.
	Messages() <-chan []byte
	// Close ends the subscription and releases its resources.
	Close() error
}

func (c *client) Publish(ctx context.Context, channel string, payload []byte) error {
	return c.rdb.Publish(ctx, channel, payload).Err()
}

func (c *client) Subscribe(ctx context.Context, channel string) (Subscription, error) {
	sub := c.rdb.Subscribe(ctx, channel)
	if _, err := sub.Receive(ctx); err != nil {
		_ = sub.Close()
		return nil, err
	}
	out := make(chan []byte, 64)
	go func() {
		defer close(out)
		for msg := range sub.Channel() {
			select {
			case out <- []byte(msg.Payload):
			default:
				// A slow consumer drops messages rather than blocking the
				// shared redis pub/sub read loop; callers needing
				// lossless delivery should keep up promptly (dispatch
				// progress/result messages are latency-sensitive by
				// nature, so blocking here would be worse).
			}
		}
	}()
	return &redisSubscription{sub: sub, out: out}, nil
}

type redisSubscription struct {
	sub *redis.PubSub
	out chan []byte
}

func (s *redisSubscription) Messages() <-chan []byte { return s.out }
func (s *redisSubscription) Close() error            { return s.sub.Close() }

var (
	_ KVStore = (*client)(nil)
	_ PubSub  = (*client)(nil)
)
