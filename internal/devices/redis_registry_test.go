package devices

import (
	"context"
	"testing"
	"time"

	"github.com/champi-ai/rc-mcp/internal/redisclient"
)

func newRedisTestRegistry(t *testing.T, ttl time.Duration) (*RedisRegistry, *redisclient.Fake) {
	t.Helper()
	kv := redisclient.NewFake()
	return NewRedisRegistry(kv, ttl), kv
}

func TestRedisRegistry_PairingFlow(t *testing.T) {
	reg, _ := newRedisTestRegistry(t, time.Minute)
	ctx := context.Background()

	pc, err := reg.CreatePairingCode(ctx, "host-1")
	if err != nil {
		t.Fatalf("CreatePairingCode: %v", err)
	}
	device, token, err := reg.ApprovePairing(ctx, pc.Code)
	if err != nil {
		t.Fatalf("ApprovePairing: %v", err)
	}
	if device.Label != "host-1" {
		t.Fatalf("label = %q, want host-1", device.Label)
	}

	got, err := reg.Authenticate(ctx, token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got.ID != device.ID {
		t.Fatalf("authenticated device = %s, want %s", got.ID, device.ID)
	}

	if _, _, err := reg.ApprovePairing(ctx, pc.Code); err != ErrPairingCodeUsed {
		t.Fatalf("re-approving a used code: err = %v, want ErrPairingCodeUsed", err)
	}
}

func TestRedisRegistry_RejectPairing(t *testing.T) {
	reg, _ := newRedisTestRegistry(t, time.Minute)
	ctx := context.Background()

	pc, err := reg.CreatePairingCode(ctx, "host-1")
	if err != nil {
		t.Fatalf("CreatePairingCode: %v", err)
	}
	if err := reg.RejectPairing(ctx, pc.Code); err != nil {
		t.Fatalf("RejectPairing: %v", err)
	}
	if _, _, err = reg.ApprovePairing(ctx, pc.Code); err != ErrPairingCodeUsed {
		t.Fatalf("approving a rejected code: err = %v, want ErrPairingCodeUsed", err)
	}
}

func TestRedisRegistry_ExpiredPairingCode(t *testing.T) {
	reg, kv := newRedisTestRegistry(t, 10*time.Millisecond)
	ctx := context.Background()

	pc, err := reg.CreatePairingCode(ctx, "host-1")
	if err != nil {
		t.Fatalf("CreatePairingCode: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	if _, _, err := reg.ApprovePairing(ctx, pc.Code); err != ErrPairingCodeNotFound && err != ErrPairingCodeExpired {
		t.Fatalf("err = %v, want ErrPairingCodeNotFound or ErrPairingCodeExpired", err)
	}
	// The Redis TTL should also have reaped the key entirely.
	keys, _ := kv.Keys(ctx, "rc-mcp:pairing:*")
	if len(keys) != 0 {
		t.Fatalf("expired pairing code key still present: %v", keys)
	}
}

func TestRedisRegistry_DeviceLifecycle(t *testing.T) {
	reg, _ := newRedisTestRegistry(t, time.Minute)
	ctx := context.Background()

	pc, err := reg.CreatePairingCode(ctx, "host-1")
	if err != nil {
		t.Fatalf("CreatePairingCode: %v", err)
	}
	device, _, err := reg.ApprovePairing(ctx, pc.Code)
	if err != nil {
		t.Fatalf("ApprovePairing: %v", err)
	}

	if err := reg.SetOnline(ctx, device.ID, true); err != nil {
		t.Fatalf("SetOnline: %v", err)
	}
	got, err := reg.Get(ctx, device.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.Online {
		t.Fatal("device should be online")
	}

	if err := reg.UpdateCapabilities(ctx, device.ID, []string{"shell", "fs"}); err != nil {
		t.Fatalf("UpdateCapabilities: %v", err)
	}
	got, _ = reg.Get(ctx, device.ID)
	if len(got.Capabilities) != 2 {
		t.Fatalf("capabilities = %v", got.Capabilities)
	}

	all, err := reg.List(ctx)
	if err != nil || len(all) != 1 {
		t.Fatalf("List: %v, %d devices", err, len(all))
	}

	if err := reg.Revoke(ctx, device.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := reg.Get(ctx, device.ID); err != ErrDeviceNotFound {
		t.Fatalf("Get after revoke: err = %v, want ErrDeviceNotFound", err)
	}
}

func TestRedisRegistry_PendingPairingCodes_ExcludesUsedAndExpired(t *testing.T) {
	reg, _ := newRedisTestRegistry(t, time.Minute)
	ctx := context.Background()

	pending, err := reg.CreatePairingCode(ctx, "pending-host")
	if err != nil {
		t.Fatalf("CreatePairingCode: %v", err)
	}
	used, err := reg.CreatePairingCode(ctx, "used-host")
	if err != nil {
		t.Fatalf("CreatePairingCode: %v", err)
	}
	if _, _, err := reg.ApprovePairing(ctx, used.Code); err != nil {
		t.Fatalf("ApprovePairing: %v", err)
	}

	codes, err := reg.PendingPairingCodes(ctx)
	if err != nil {
		t.Fatalf("PendingPairingCodes: %v", err)
	}
	if len(codes) != 1 || codes[0].Code != pending.Code {
		t.Fatalf("codes = %+v, want only %q", codes, pending.Code)
	}
}

// TestRedisRegistry_ConsistentAcrossTwoInstances exercises the acceptance
// criterion directly: two RedisRegistry values sharing the same backing
// store (standing in for two server replicas pointed at the same Redis)
// see each other's writes.
func TestRedisRegistry_ConsistentAcrossTwoInstances(t *testing.T) {
	kv := redisclient.NewFake()
	replicaA := NewRedisRegistry(kv, time.Minute)
	replicaB := NewRedisRegistry(kv, time.Minute)
	ctx := context.Background()

	pc, err := replicaA.CreatePairingCode(ctx, "host-1")
	if err != nil {
		t.Fatalf("CreatePairingCode: %v", err)
	}
	// A different replica approves the code created on replica A.
	device, _, err := replicaB.ApprovePairing(ctx, pc.Code)
	if err != nil {
		t.Fatalf("ApprovePairing on replica B: %v", err)
	}

	// Replica A immediately sees the device replica B just created.
	got, err := replicaA.Get(ctx, device.ID)
	if err != nil {
		t.Fatalf("Get on replica A: %v", err)
	}
	if got.ID != device.ID {
		t.Fatalf("got = %+v, want device %s", got, device.ID)
	}

	if err := replicaA.SetOnline(ctx, device.ID, true); err != nil {
		t.Fatalf("SetOnline on replica A: %v", err)
	}
	got, err = replicaB.Get(ctx, device.ID)
	if err != nil {
		t.Fatalf("Get on replica B: %v", err)
	}
	if !got.Online {
		t.Fatal("replica B should see the online status set by replica A")
	}
}

func TestRedisRegistry_AuthenticateUnknownToken(t *testing.T) {
	reg, _ := newRedisTestRegistry(t, time.Minute)
	if _, err := reg.Authenticate(context.Background(), "no-such-token"); err != ErrAuthFailed {
		t.Fatalf("err = %v, want ErrAuthFailed", err)
	}
}

func TestRedisRegistry_UnknownPairingCode(t *testing.T) {
	reg, _ := newRedisTestRegistry(t, time.Minute)
	ctx := context.Background()
	if _, _, err := reg.ApprovePairing(ctx, "GHOST-0000"); err != ErrPairingCodeNotFound {
		t.Fatalf("err = %v, want ErrPairingCodeNotFound", err)
	}
	if err := reg.RejectPairing(ctx, "GHOST-0000"); err != ErrPairingCodeNotFound {
		t.Fatalf("err = %v, want ErrPairingCodeNotFound", err)
	}
}
