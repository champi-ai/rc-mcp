package devices

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestRegistry(t *testing.T) (*FileRegistry, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "devices.json")
	r, err := NewFileRegistry(path)
	if err != nil {
		t.Fatalf("NewFileRegistry: %v", err)
	}
	return r, path
}

func TestCreateApproveAuthenticateRevoke_RoundTrip(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRegistry(t)

	pc, err := r.CreatePairingCode(ctx, "my-laptop")
	if err != nil {
		t.Fatalf("CreatePairingCode: %v", err)
	}
	if pc.Used {
		t.Fatal("new pairing code should not be used")
	}

	device, token, err := r.ApprovePairing(ctx, pc.Code)
	if err != nil {
		t.Fatalf("ApprovePairing: %v", err)
	}
	if device.ID == "" {
		t.Fatal("expected a non-empty device ID")
	}
	if token == "" {
		t.Fatal("expected a non-empty raw device token")
	}
	if device.Label != "my-laptop" {
		t.Errorf("label = %q, want %q", device.Label, "my-laptop")
	}

	got, err := r.Authenticate(ctx, token)
	if err != nil {
		t.Fatalf("Authenticate with valid token: %v", err)
	}
	if got.ID != device.ID {
		t.Errorf("authenticated device ID = %q, want %q", got.ID, device.ID)
	}

	if err := r.Revoke(ctx, device.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	if _, err := r.Authenticate(ctx, token); err == nil {
		t.Fatal("expected Authenticate to fail after revocation")
	}

	if _, err := r.Get(ctx, device.ID); err == nil {
		t.Fatal("expected Get to fail after revocation")
	}
}

func TestApprovePairing_SingleUse(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRegistry(t)

	pc, err := r.CreatePairingCode(ctx, "host")
	if err != nil {
		t.Fatalf("CreatePairingCode: %v", err)
	}

	if _, _, err := r.ApprovePairing(ctx, pc.Code); err != nil {
		t.Fatalf("first ApprovePairing: %v", err)
	}
	if _, _, err := r.ApprovePairing(ctx, pc.Code); err != ErrPairingCodeUsed {
		t.Fatalf("second ApprovePairing: got %v, want ErrPairingCodeUsed", err)
	}
}

func TestApprovePairing_Expired(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	r, err := NewFileRegistryWithTTL(filepath.Join(dir, "devices.json"), 1*time.Millisecond)
	if err != nil {
		t.Fatalf("NewFileRegistryWithTTL: %v", err)
	}

	pc, err := r.CreatePairingCode(ctx, "host")
	if err != nil {
		t.Fatalf("CreatePairingCode: %v", err)
	}

	time.Sleep(5 * time.Millisecond)

	if _, _, err := r.ApprovePairing(ctx, pc.Code); err != ErrPairingCodeExpired {
		t.Fatalf("ApprovePairing after TTL: got %v, want ErrPairingCodeExpired", err)
	}
}

func TestRejectPairing(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRegistry(t)

	pc, err := r.CreatePairingCode(ctx, "host")
	if err != nil {
		t.Fatalf("CreatePairingCode: %v", err)
	}
	if err := r.RejectPairing(ctx, pc.Code); err != nil {
		t.Fatalf("RejectPairing: %v", err)
	}
	if _, _, err := r.ApprovePairing(ctx, pc.Code); err != ErrPairingCodeUsed {
		t.Fatalf("ApprovePairing after reject: got %v, want ErrPairingCodeUsed", err)
	}
}

func TestTokensPersistedAsBcryptHashOnly(t *testing.T) {
	ctx := context.Background()
	r, path := newTestRegistry(t)

	pc, err := r.CreatePairingCode(ctx, "host")
	if err != nil {
		t.Fatalf("CreatePairingCode: %v", err)
	}
	_, token, err := r.ApprovePairing(ctx, pc.Code)
	if err != nil {
		t.Fatalf("ApprovePairing: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read registry file: %v", err)
	}
	if strings.Contains(string(raw), token) {
		t.Fatal("raw device token must never appear in the persisted registry file")
	}

	var f registryFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("unmarshal registry file: %v", err)
	}
	if len(f.Devices) != 1 {
		t.Fatalf("expected 1 device on disk, got %d", len(f.Devices))
	}
	if f.Devices[0].TokenHash == "" || f.Devices[0].TokenHash == token {
		t.Fatal("expected a non-empty bcrypt hash distinct from the raw token")
	}
	if !strings.HasPrefix(f.Devices[0].TokenHash, "$2") {
		t.Fatalf("expected a bcrypt hash prefix, got %q", f.Devices[0].TokenHash)
	}
}

func TestConcurrentWrites_DoNotCorruptFile(t *testing.T) {
	ctx := context.Background()
	r, path := newTestRegistry(t)

	pc, err := r.CreatePairingCode(ctx, "host")
	if err != nil {
		t.Fatalf("CreatePairingCode: %v", err)
	}
	device, _, err := r.ApprovePairing(ctx, pc.Code)
	if err != nil {
		t.Fatalf("ApprovePairing: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			_ = r.SetOnline(ctx, device.ID, i%2 == 0)
		}(i)
		go func(i int) {
			defer wg.Done()
			_ = r.UpdateCapabilities(ctx, device.ID, []string{"shell"})
		}(i)
	}
	wg.Wait()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read registry file after concurrent writes: %v", err)
	}
	var f registryFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("registry file corrupted after concurrent writes: %v", err)
	}
	if len(f.Devices) != 1 {
		t.Fatalf("expected 1 device after concurrent writes, got %d", len(f.Devices))
	}
}

func TestList(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRegistry(t)

	for _, host := range []string{"a", "b", "c"} {
		pc, err := r.CreatePairingCode(ctx, host)
		if err != nil {
			t.Fatalf("CreatePairingCode(%s): %v", host, err)
		}
		if _, _, err := r.ApprovePairing(ctx, pc.Code); err != nil {
			t.Fatalf("ApprovePairing(%s): %v", host, err)
		}
	}

	devices, err := r.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(devices) != 3 {
		t.Fatalf("List returned %d devices, want 3", len(devices))
	}
}

func TestAuthenticate_UnknownToken(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRegistry(t)

	if _, err := r.Authenticate(ctx, "not-a-real-token"); err != ErrAuthFailed {
		t.Fatalf("Authenticate with unknown token: got %v, want ErrAuthFailed", err)
	}
}
