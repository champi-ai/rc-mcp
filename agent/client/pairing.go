package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/champi-ai/rc-mcp/internal/protocol"
)

// ErrPairingExpired is returned by Pair when the server closes the
// connection because the pairing code expired before an operator approved
// it. Callers should surface this clearly and may retry pairing.
var ErrPairingExpired = errors.New("pairing code expired before approval")

// ErrPairingRejected is returned by Pair when an operator explicitly
// rejects the pairing code via the admin API.
var ErrPairingRejected = errors.New("pairing code was rejected")

// PairResult holds the outcome of a successful first-run pairing.
type PairResult struct {
	DeviceID    string
	DeviceToken string
}

// Pair runs the first-run pairing flow (docs/specs/backend.md Section
// 12.2): dial, send pair_request, print the pairing code to out, then wait
// for pair_approved. It does not persist the token -- call SaveToken with
// the result.
func (c *Client) Pair(ctx context.Context, hostname string, out io.Writer) (*PairResult, error) {
	conn, err := c.DialRaw(ctx)
	if err != nil {
		return nil, fmt.Errorf("dial for pairing: %w", err)
	}
	defer conn.Close()

	if err := conn.WriteJSON(protocol.Envelope{
		Type: protocol.MsgPairRequest,
		Ts:   time.Now().UTC(),
		Payload: protocol.PairRequestPayload{
			Hostname: hostname,
		},
	}); err != nil {
		return nil, fmt.Errorf("send pair_request: %w", err)
	}

	// Let a context cancellation interrupt a blocked read.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()

	var env protocol.Envelope
	if err := conn.ReadJSON(&env); err != nil {
		return nil, fmt.Errorf("read pair_code: %w", err)
	}
	if env.Type != protocol.MsgPairCode {
		return nil, fmt.Errorf("expected pair_code, got %s", env.Type)
	}
	codePayload, err := decodePayload[protocol.PairCodePayload](env.Payload)
	if err != nil {
		return nil, fmt.Errorf("decode pair_code: %w", err)
	}

	_, _ = fmt.Fprintf(out, "\nPairing code: %s\n", codePayload.Code)
	_, _ = fmt.Fprintf(out, "Expires at:   %s\n", codePayload.ExpiresAt.Format(time.RFC3339))
	_, _ = fmt.Fprintf(out, "Approve on the server with: curl -X POST http://127.0.0.1:9090/admin/approve -d '{\"code\":\"%s\"}'\n\n", codePayload.Code)

	for {
		var next protocol.Envelope
		if err := conn.ReadJSON(&next); err != nil {
			return nil, fmt.Errorf("read pairing response: %w", err)
		}

		switch next.Type {
		case protocol.MsgPairApproved:
			approved, err := decodePayload[protocol.PairApprovedPayload](next.Payload)
			if err != nil {
				return nil, fmt.Errorf("decode pair_approved: %w", err)
			}
			return &PairResult{DeviceID: approved.DeviceID, DeviceToken: approved.DeviceToken}, nil

		case protocol.MsgError:
			errPayload, err := decodePayload[protocol.ErrorPayload](next.Payload)
			if err != nil {
				return nil, fmt.Errorf("decode error payload: %w", err)
			}
			switch errPayload.Code {
			case "pairing_expired":
				_, _ = fmt.Fprintln(out, "Pairing code expired before it was approved. Restarting pairing...")
				return nil, ErrPairingExpired
			case "pairing_rejected":
				_, _ = fmt.Fprintln(out, "Pairing code was rejected by the operator.")
				return nil, ErrPairingRejected
			default:
				return nil, fmt.Errorf("pairing failed: %s: %s", errPayload.Code, errPayload.Message)
			}

		default:
			// Ignore unrelated messages (e.g. stray pings) while waiting.
			continue
		}
	}
}

// DefaultTokenPath returns ~/.rc-mcp/agent-token, the default
// AGENT_TOKEN_PATH.
func DefaultTokenPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".rc-mcp", "agent-token"), nil
}

// SaveToken persists a device token to path with mode 0600, creating any
// parent directories as needed.
func SaveToken(path, token string) error {
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create token directory: %w", err)
		}
	}
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		return fmt.Errorf("write token file: %w", err)
	}
	// os.WriteFile respects umask; enforce the mode explicitly since the
	// token must never be group/world readable.
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("chmod token file: %w", err)
	}
	return nil
}

// LoadToken reads a persisted device token from path. ok is false (with a
// nil error) if no token file exists yet, signaling that first-run pairing
// should run.
func LoadToken(path string) (token string, ok bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read token file: %w", err)
	}
	return string(data), true, nil
}
