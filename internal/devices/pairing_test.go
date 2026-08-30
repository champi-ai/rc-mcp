package devices

import (
	"context"
	"regexp"
	"testing"
	"time"
)

var pairingCodeFormat = regexp.MustCompile(`^[ABCDEFGHJKMNPQRSTUVWXYZ23456789]{4}-[ABCDEFGHJKMNPQRSTUVWXYZ23456789]{4}$`)

func TestGeneratePairingCode_Format(t *testing.T) {
	for i := 0; i < 200; i++ {
		code, err := generatePairingCode()
		if err != nil {
			t.Fatalf("generatePairingCode: %v", err)
		}
		if !pairingCodeFormat.MatchString(code) {
			t.Fatalf("code %q does not match XXXX-XXXX with no ambiguous characters", code)
		}
		for _, ambiguous := range []rune{'0', 'O', '1', 'I', 'L'} {
			for _, r := range code {
				if r == ambiguous {
					t.Fatalf("code %q contains ambiguous character %q", code, ambiguous)
				}
			}
		}
	}
}

func TestGeneratePairingCode_Uniqueness(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		code, err := generatePairingCode()
		if err != nil {
			t.Fatalf("generatePairingCode: %v", err)
		}
		if seen[code] {
			// crypto/rand over a 32^8 space; a collision in 500 draws would
			// indicate a broken RNG, not bad luck.
			t.Fatalf("unexpected duplicate pairing code %q", code)
		}
		seen[code] = true
	}
}

func TestPairingCode_IsExpired(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pc := &PairingCode{ExpiresAt: now.Add(5 * time.Minute)}

	if pc.IsExpired(now) {
		t.Error("code should not be expired before its TTL")
	}
	if !pc.IsExpired(now.Add(5*time.Minute + time.Second)) {
		t.Error("code should be expired after its TTL")
	}
}

func TestPairingCodeTTL_UnapprovedPastTTL(t *testing.T) {
	ctx := context.Background()
	r, err := NewFileRegistryWithTTL(t.TempDir()+"/devices.json", 10*time.Millisecond)
	if err != nil {
		t.Fatalf("NewFileRegistryWithTTL: %v", err)
	}

	pc, err := r.CreatePairingCode(ctx, "host")
	if err != nil {
		t.Fatalf("CreatePairingCode: %v", err)
	}

	time.Sleep(20 * time.Millisecond)

	pending, err := r.PendingPairingCodes(ctx)
	if err != nil {
		t.Fatalf("PendingPairingCodes: %v", err)
	}
	for _, p := range pending {
		if p.Code == pc.Code {
			t.Fatalf("expired code %q must not appear in pending codes", pc.Code)
		}
	}

	if _, _, err := r.ApprovePairing(ctx, pc.Code); err != ErrPairingCodeExpired {
		t.Fatalf("ApprovePairing on expired code: got %v, want ErrPairingCodeExpired", err)
	}
}

func TestPairingCodeSingleUse_ApproveTwice(t *testing.T) {
	ctx := context.Background()
	r, err := NewFileRegistry(t.TempDir() + "/devices.json")
	if err != nil {
		t.Fatalf("NewFileRegistry: %v", err)
	}

	pc, err := r.CreatePairingCode(ctx, "host")
	if err != nil {
		t.Fatalf("CreatePairingCode: %v", err)
	}
	if _, _, err := r.ApprovePairing(ctx, pc.Code); err != nil {
		t.Fatalf("first approval: %v", err)
	}
	if _, _, err := r.ApprovePairing(ctx, pc.Code); err != ErrPairingCodeUsed {
		t.Fatalf("second approval: got %v, want ErrPairingCodeUsed", err)
	}
}
