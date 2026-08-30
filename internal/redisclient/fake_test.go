package redisclient

import (
	"context"
	"testing"
	"time"
)

func TestFake_SetGetDel(t *testing.T) {
	f := NewFake()
	ctx := context.Background()

	if err := f.Set(ctx, "k", "v", 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	v, err := f.Get(ctx, "k")
	if err != nil || v != "v" {
		t.Fatalf("Get = (%q, %v)", v, err)
	}
	if err := f.Del(ctx, "k"); err != nil {
		t.Fatalf("Del: %v", err)
	}
	if _, err := f.Get(ctx, "k"); err != ErrKeyNotFound {
		t.Fatalf("err = %v, want ErrKeyNotFound", err)
	}
}

func TestFake_TTLExpiry(t *testing.T) {
	f := NewFake()
	ctx := context.Background()

	if err := f.Set(ctx, "k", "v", 20*time.Millisecond); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, err := f.Get(ctx, "k"); err != nil {
		t.Fatalf("Get before expiry: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := f.Get(ctx, "k"); err != ErrKeyNotFound {
		t.Fatalf("err after expiry = %v, want ErrKeyNotFound", err)
	}
}

func TestFake_ExpireRefreshesTTL(t *testing.T) {
	f := NewFake()
	ctx := context.Background()

	if err := f.Set(ctx, "k", "v", 20*time.Millisecond); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := f.Expire(ctx, "k", time.Minute); err != nil {
		t.Fatalf("Expire: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := f.Get(ctx, "k"); err != nil {
		t.Fatalf("Get after Expire refresh: %v", err)
	}
}

func TestFake_ExpireOnMissingKeyIsNoop(t *testing.T) {
	f := NewFake()
	if err := f.Expire(context.Background(), "no-such-key", time.Minute); err != nil {
		t.Fatalf("Expire on missing key: %v", err)
	}
}

func TestFake_KeysGlobPattern(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	_ = f.Set(ctx, "device:1", "a", 0)
	_ = f.Set(ctx, "device:2", "b", 0)
	_ = f.Set(ctx, "pairing:1", "c", 0)

	keys, err := f.Keys(ctx, "device:*")
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("keys = %v, want 2 device:* matches", keys)
	}
}

func TestFake_DelMultipleAndMissing(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	_ = f.Set(ctx, "a", "1", 0)
	_ = f.Set(ctx, "b", "2", 0)
	if err := f.Del(ctx, "a", "b", "missing"); err != nil {
		t.Fatalf("Del: %v", err)
	}
	keys, _ := f.Keys(ctx, "*")
	if len(keys) != 0 {
		t.Fatalf("keys = %v, want empty", keys)
	}
}

func TestFake_PublishSubscribe(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	sub, err := f.Subscribe(ctx, "chan-1")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	if err := f.Publish(ctx, "chan-1", []byte("hello")); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case msg := <-sub.Messages():
		if string(msg) != "hello" {
			t.Fatalf("msg = %q, want hello", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for message")
	}
}

func TestFake_NewHandle_SharesStateAcrossPubSub(t *testing.T) {
	replicaA := NewFake()
	replicaB := replicaA.NewHandle()
	ctx := context.Background()

	sub, err := replicaB.Subscribe(ctx, "cross-replica")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	if err := replicaA.Publish(ctx, "cross-replica", []byte("from-a")); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case msg := <-sub.Messages():
		if string(msg) != "from-a" {
			t.Fatalf("msg = %q, want from-a", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("replica B never received replica A's publish")
	}
}

func TestFake_NewHandle_SharesKVState(t *testing.T) {
	replicaA := NewFake()
	replicaB := replicaA.NewHandle()
	ctx := context.Background()

	if err := replicaA.Set(ctx, "k", "v", 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := replicaB.Get(ctx, "k")
	if err != nil || got != "v" {
		t.Fatalf("replica B Get = (%q, %v), want (v, nil)", got, err)
	}
}

func TestFake_UnsubscribeStopsDelivery(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	sub, err := f.Subscribe(ctx, "chan-1")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := sub.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := f.Publish(ctx, "chan-1", []byte("after-close")); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case msg, ok := <-sub.Messages():
		if ok {
			t.Fatalf("received a message after unsubscribe: %q", msg)
		}
	case <-time.After(100 * time.Millisecond):
	}
}
