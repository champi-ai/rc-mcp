// Package devices implements the device registry: the durable record of
// every paired desktop agent, plus the pairing-code lifecycle used to
// enroll new devices. See docs/specs/backend.md Section 6 and Section 12.2.
package devices

import (
	"context"
	"time"
)

// Device represents a paired desktop agent in the registry.
type Device struct {
	ID           string    `json:"clientId"` // server-assigned UUIDv4; serialized as clientId for consistency with every tool's target-device field
	Label        string    `json:"label"`    // human-readable (typically hostname)
	TokenHash    string    `json:"-"`        // bcrypt hash of the device token; never serialized
	Online       bool      `json:"online"`
	Capabilities []string  `json:"capabilities"` // enabled capability areas
	LastSeen     time.Time `json:"lastSeen"`
	PairedAt     time.Time `json:"pairedAt"`
}

// PairingCode represents a pending pairing request.
type PairingCode struct {
	Code      string    `json:"code"`     // human-readable, e.g. "ABCD-1234"
	Hostname  string    `json:"hostname"` // agent-reported hostname
	ExpiresAt time.Time `json:"expiresAt"`
	Used      bool      `json:"used"` // single-use; true after approval or rejection
}

// DeviceRegistry is the interface for managing paired devices.
type DeviceRegistry interface {
	// CreatePairingCode creates a new pending pairing code.
	CreatePairingCode(ctx context.Context, hostname string) (*PairingCode, error)
	// ApprovePairing approves a pairing code and registers the device.
	// Returns the new Device and a raw (unhashed) device token.
	ApprovePairing(ctx context.Context, code string) (*Device, string, error)
	// RejectPairing rejects (invalidates) a pairing code.
	RejectPairing(ctx context.Context, code string) error
	// Authenticate validates a device token and returns the device.
	Authenticate(ctx context.Context, token string) (*Device, error)
	// Get returns a device by ID.
	Get(ctx context.Context, id string) (*Device, error)
	// List returns all paired devices.
	List(ctx context.Context) ([]*Device, error)
	// SetOnline marks a device as online/offline and updates LastSeen.
	SetOnline(ctx context.Context, id string, online bool) error
	// UpdateCapabilities updates the capability list for a device.
	UpdateCapabilities(ctx context.Context, id string, caps []string) error
	// Revoke removes a device from the registry (operator-initiated).
	Revoke(ctx context.Context, id string) error
	// PendingPairingCodes returns all non-expired, unused pairing codes.
	PendingPairingCodes(ctx context.Context) ([]*PairingCode, error)
}
