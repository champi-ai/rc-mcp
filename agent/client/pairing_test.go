package client

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/champi-ai/rc-mcp/internal/protocol"
)

// newPairingServer runs a minimal server that replies to pair_request with
// pair_code, then sends whatever `then` produces after a short delay.
func newPairingServer(t *testing.T, then func(env protocol.Envelope) protocol.Envelope) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		var req protocol.Envelope
		if err := conn.ReadJSON(&req); err != nil {
			return
		}

		_ = conn.WriteJSON(protocol.Envelope{
			Type: protocol.MsgPairCode,
			Ts:   time.Now().UTC(),
			Payload: protocol.PairCodePayload{
				Code:      "ABCD-1234",
				ExpiresAt: time.Now().Add(5 * time.Minute),
			},
		})

		resp := then(req)
		_ = conn.WriteJSON(resp)
	}))
}

func TestPair_Success(t *testing.T) {
	srv := newPairingServer(t, func(req protocol.Envelope) protocol.Envelope {
		return protocol.Envelope{
			Type: protocol.MsgPairApproved,
			Ts:   time.Now().UTC(),
			Payload: protocol.PairApprovedPayload{
				DeviceID:    "device-xyz",
				DeviceToken: "raw-token-value",
			},
		}
	})
	defer srv.Close()

	c := NewClient(wsURL(srv))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var out bytes.Buffer
	res, err := c.Pair(ctx, "test-host", &out)
	if err != nil {
		t.Fatalf("Pair: %v", err)
	}
	if res.DeviceID != "device-xyz" || res.DeviceToken != "raw-token-value" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if !bytes.Contains(out.Bytes(), []byte("ABCD-1234")) {
		t.Errorf("expected pairing code printed to stdout, got: %s", out.String())
	}
}

func TestPair_Expired(t *testing.T) {
	srv := newPairingServer(t, func(req protocol.Envelope) protocol.Envelope {
		return protocol.Envelope{
			Type: protocol.MsgError,
			Ts:   time.Now().UTC(),
			Payload: protocol.ErrorPayload{
				Code:    "pairing_expired",
				Message: "pairing code expired",
			},
		}
	})
	defer srv.Close()

	c := NewClient(wsURL(srv))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var out bytes.Buffer
	_, err := c.Pair(ctx, "test-host", &out)
	if err != ErrPairingExpired {
		t.Fatalf("got %v, want ErrPairingExpired", err)
	}
}

func TestPair_Rejected(t *testing.T) {
	srv := newPairingServer(t, func(req protocol.Envelope) protocol.Envelope {
		return protocol.Envelope{
			Type: protocol.MsgError,
			Ts:   time.Now().UTC(),
			Payload: protocol.ErrorPayload{
				Code:    "pairing_rejected",
				Message: "rejected by operator",
			},
		}
	})
	defer srv.Close()

	c := NewClient(wsURL(srv))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var out bytes.Buffer
	_, err := c.Pair(ctx, "test-host", &out)
	if err != ErrPairingRejected {
		t.Fatalf("got %v, want ErrPairingRejected", err)
	}
}

func TestSaveAndLoadToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "agent-token")

	if err := SaveToken(path, "super-secret-token"); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat token file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("token file mode = %v, want 0600", info.Mode().Perm())
	}

	token, ok, err := LoadToken(path)
	if err != nil {
		t.Fatalf("LoadToken: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true for an existing token file")
	}
	if token != "super-secret-token" {
		t.Errorf("token = %q, want %q", token, "super-secret-token")
	}
}

func TestLoadToken_MissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist")

	token, ok, err := LoadToken(path)
	if err != nil {
		t.Fatalf("LoadToken: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false for a missing token file")
	}
	if token != "" {
		t.Errorf("expected empty token, got %q", token)
	}
}

func TestDefaultTokenPath(t *testing.T) {
	path, err := DefaultTokenPath()
	if err != nil {
		t.Fatalf("DefaultTokenPath: %v", err)
	}
	if filepath.Base(path) != "agent-token" {
		t.Errorf("expected file named agent-token, got %q", path)
	}
	if filepath.Base(filepath.Dir(path)) != ".rc-mcp" {
		t.Errorf("expected parent dir .rc-mcp, got %q", filepath.Dir(path))
	}
}
