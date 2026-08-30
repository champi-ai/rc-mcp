package session

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/CloudKeter/rc-mcp/internal/redisclient"
)

func TestRedisStore_CreateMirrorsToRedis(t *testing.T) {
	kv := redisclient.NewFake()
	store := NewRedisStore(kv, 10, time.Minute)

	sess, err := store.Create(context.Background())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	data, err := kv.Get(context.Background(), "rc-mcp:session:"+sess.ID)
	if err != nil {
		t.Fatalf("Redis mirror missing: %v", err)
	}
	var meta sessionMeta
	if err := json.Unmarshal([]byte(data), &meta); err != nil {
		t.Fatalf("unmarshal mirror: %v", err)
	}
	if meta.ID != sess.ID {
		t.Fatalf("meta.ID = %q, want %q", meta.ID, sess.ID)
	}
}

func TestRedisStore_GetDelegatesToLocalMemory(t *testing.T) {
	kv := redisclient.NewFake()
	store := NewRedisStore(kv, 10, time.Minute)

	sess, err := store.Create(context.Background())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := store.Get(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != sess {
		t.Fatal("Get should return the same live Session instance Create returned")
	}

	if _, err := store.Get(context.Background(), "no-such-id"); err != ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestRedisStore_DeleteRemovesLocalAndMirror(t *testing.T) {
	kv := redisclient.NewFake()
	store := NewRedisStore(kv, 10, time.Minute)

	sess, err := store.Create(context.Background())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Delete(context.Background(), sess.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, err := store.Get(context.Background(), sess.ID); err != ErrNotFound {
		t.Fatalf("Get after delete: err = %v, want ErrNotFound", err)
	}
	if _, err := kv.Get(context.Background(), "rc-mcp:session:"+sess.ID); err != redisclient.ErrKeyNotFound {
		t.Fatalf("Redis mirror after delete: err = %v, want ErrKeyNotFound", err)
	}
	select {
	case <-sess.Ctx.Done():
	default:
		t.Fatal("Delete should cancel the session's context")
	}
}

func TestRedisStore_TouchRefreshesRedisTTL(t *testing.T) {
	kv := redisclient.NewFake()
	store := NewRedisStore(kv, 10, 50*time.Millisecond)

	sess, err := store.Create(context.Background())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Keep touching faster than the TTL elapses; the mirror should never
	// expire out from under an active session.
	for i := 0; i < 3; i++ {
		time.Sleep(20 * time.Millisecond)
		if err := store.Touch(context.Background(), sess.ID); err != nil {
			t.Fatalf("Touch: %v", err)
		}
	}
	if _, err := kv.Get(context.Background(), "rc-mcp:session:"+sess.ID); err != nil {
		t.Fatalf("mirror expired despite repeated Touch: %v", err)
	}

	if err := store.Touch(context.Background(), "no-such-id"); err != ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestRedisStore_RangeDelegatesToLocalMemory(t *testing.T) {
	kv := redisclient.NewFake()
	store := NewRedisStore(kv, 10, time.Minute)

	if _, err := store.Create(context.Background()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := store.Create(context.Background()); err != nil {
		t.Fatalf("Create: %v", err)
	}

	count := 0
	store.Range(func(*Session) bool {
		count++
		return true
	})
	if count != 2 {
		t.Fatalf("Range visited %d sessions, want 2", count)
	}
}
