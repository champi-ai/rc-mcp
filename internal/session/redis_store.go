package session

import (
	"context"
	"encoding/json"
	"time"

	"github.com/champi-ai/rc-mcp/internal/redisclient"
)

const sessionKeyPrefix = "rc-mcp:session:"

// RedisStore is a SessionStore that mirrors session metadata to Redis with
// a TTL matching the idle timeout (docs/specs/backend.md Section 7,
// Section 19: "multi-replica"), so a replica (or an operator) can see
// which sessions exist across the whole cluster and when they'll expire.
//
// The live Session itself -- its SSE event channel, replay buffer, and
// context -- is inherently process-local and is NOT reconstructable from
// the Redis mirror; RedisStore delegates all of that to an embedded
// MemoryStore. This means Get only ever finds a session created on *this*
// replica: making a session usable from any replica requires the
// cross-replica dispatch routing follow-up (Section 10), which this store
// change alone does not provide, per the issue's own scope note.
type RedisStore struct {
	inner *MemoryStore
	kv    redisclient.KVStore
	ttl   time.Duration
}

// sessionMeta is what gets mirrored to Redis: enough to answer "does this
// session exist, and when does it expire" without needing (or being able
// to reconstruct) the live process-local Session.
type sessionMeta struct {
	ID                string    `json:"id"`
	CreatedAt         time.Time `json:"createdAt"`
	NegotiatedVersion string    `json:"negotiatedVersion,omitempty"`
	ClientName        string    `json:"clientName,omitempty"`
	ClientVersion     string    `json:"clientVersion,omitempty"`
	LastActivityAt    time.Time `json:"lastActivityAt"`
}

// NewRedisStore constructs a RedisStore. replayBufferSize is passed
// through to the embedded MemoryStore (<=0 uses DefaultReplayBufferSize).
// idleTimeout <=0 uses DefaultIdleTimeout as the mirrored Redis TTL,
// matching MCP_SESSION_IDLE_TIMEOUT.
func NewRedisStore(kv redisclient.KVStore, replayBufferSize int, idleTimeout time.Duration) *RedisStore {
	if idleTimeout <= 0 {
		idleTimeout = DefaultIdleTimeout
	}
	return &RedisStore{
		inner: NewMemoryStore(replayBufferSize),
		kv:    kv,
		ttl:   idleTimeout,
	}
}

func (s *RedisStore) Create(ctx context.Context) (*Session, error) {
	sess, err := s.inner.Create(ctx)
	if err != nil {
		return nil, err
	}
	s.mirror(ctx, sess)
	return sess, nil
}

func (s *RedisStore) Get(ctx context.Context, id string) (*Session, error) {
	return s.inner.Get(ctx, id)
}

func (s *RedisStore) Touch(ctx context.Context, id string) error {
	if err := s.inner.Touch(ctx, id); err != nil {
		return err
	}
	// Refresh the Redis TTL so a still-active session's cluster-visible
	// mirror doesn't expire out from under it. A failed refresh here is
	// logged-worthy but not request-fatal: the mirror is observability,
	// not the source of truth for this replica's own liveness tracking.
	_ = s.kv.Expire(ctx, sessionKeyPrefix+id, s.ttl)
	return nil
}

func (s *RedisStore) Delete(ctx context.Context, id string) error {
	if err := s.inner.Delete(ctx, id); err != nil {
		return err
	}
	_ = s.kv.Del(ctx, sessionKeyPrefix+id)
	return nil
}

func (s *RedisStore) Range(fn func(*Session) bool) {
	s.inner.Range(fn)
}

// mirror writes sess's metadata to Redis with the configured TTL. Best
// effort: a mirror failure does not fail session creation, since the
// authoritative local Session was already created successfully.
func (s *RedisStore) mirror(ctx context.Context, sess *Session) {
	meta := sessionMeta{
		ID:                sess.ID,
		CreatedAt:         sess.CreatedAt,
		NegotiatedVersion: sess.NegotiatedVersion,
		ClientName:        sess.ClientInfo.Name,
		ClientVersion:     sess.ClientInfo.Version,
		LastActivityAt:    sess.LastActivityAt(),
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return
	}
	_ = s.kv.Set(ctx, sessionKeyPrefix+sess.ID, string(data), s.ttl)
}

var _ SessionStore = (*RedisStore)(nil)
