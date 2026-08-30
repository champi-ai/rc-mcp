package devices

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Sentinel errors returned by FileRegistry.
var (
	ErrPairingCodeNotFound = errors.New("pairing code not found")
	ErrPairingCodeUsed     = errors.New("pairing code already used")
	ErrPairingCodeExpired  = errors.New("pairing code expired")
	ErrDeviceNotFound      = errors.New("device not found")
	ErrAuthFailed          = errors.New("authentication failed")
)

// DefaultPairingCodeTTL is used when a registry is constructed without an
// explicit TTL (see NewFileRegistry / WithPairingCodeTTL).
const DefaultPairingCodeTTL = 5 * time.Minute

// deviceRecord is the on-disk representation of a Device, including its
// bcrypt token hash (which Device itself never serializes).
type deviceRecord struct {
	Device
	TokenHash string `json:"tokenHash"`
}

// registryFile is the full on-disk JSON document.
type registryFile struct {
	Devices      []*deviceRecord `json:"devices"`
	PairingCodes []*PairingCode  `json:"pairingCodes"`
}

// FileRegistry is a JSON-file-backed implementation of DeviceRegistry.
// All reads/writes to the backing file are protected by mu so concurrent
// goroutines (pairing approval, heartbeat SetOnline, etc.) never corrupt
// the file.
type FileRegistry struct {
	path       string
	pairingTTL time.Duration

	mu      sync.Mutex
	devices map[string]*deviceRecord // keyed by Device.ID
	codes   map[string]*PairingCode  // keyed by PairingCode.Code
}

// NewFileRegistry loads (or initializes) a FileRegistry backed by the JSON
// file at path, using DefaultPairingCodeTTL for new pairing codes.
func NewFileRegistry(path string) (*FileRegistry, error) {
	return NewFileRegistryWithTTL(path, DefaultPairingCodeTTL)
}

// NewFileRegistryWithTTL is like NewFileRegistry but with an explicit
// pairing code TTL (see PAIRING_CODE_TTL).
func NewFileRegistryWithTTL(path string, ttl time.Duration) (*FileRegistry, error) {
	r := &FileRegistry{
		path:       path,
		pairingTTL: ttl,
		devices:    map[string]*deviceRecord{},
		codes:      map[string]*PairingCode{},
	}
	if err := r.load(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *FileRegistry) load() error {
	data, err := os.ReadFile(r.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil // start empty; file is created on first write
	}
	if err != nil {
		return fmt.Errorf("read device registry: %w", err)
	}
	if len(data) == 0 {
		return nil
	}

	var f registryFile
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("parse device registry: %w", err)
	}
	for _, d := range f.Devices {
		r.devices[d.ID] = d
	}
	for _, c := range f.PairingCodes {
		r.codes[c.Code] = c
	}
	return nil
}

// saveLocked persists the current in-memory state to disk. Caller must
// hold r.mu. Writes atomically via a temp file + rename so a crash mid-write
// never leaves a corrupt registry file.
func (r *FileRegistry) saveLocked() error {
	f := registryFile{}
	for _, d := range r.devices {
		f.Devices = append(f.Devices, d)
	}
	for _, c := range r.codes {
		f.PairingCodes = append(f.PairingCodes, c)
	}

	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal device registry: %w", err)
	}

	dir := filepath.Dir(r.path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create device registry dir: %w", err)
		}
	}

	tmp, err := os.CreateTemp(dir, ".devices-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create temp registry file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once renamed

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp registry file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp registry file: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return fmt.Errorf("chmod temp registry file: %w", err)
	}
	if err := os.Rename(tmpPath, r.path); err != nil {
		return fmt.Errorf("rename temp registry file: %w", err)
	}
	return nil
}

// CreatePairingCode creates a new pending pairing code for an agent
// identifying itself with hostname.
func (r *FileRegistry) CreatePairingCode(ctx context.Context, hostname string) (*PairingCode, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var code string
	for i := 0; i < 10; i++ {
		c, err := generatePairingCode()
		if err != nil {
			return nil, err
		}
		if _, exists := r.codes[c]; !exists {
			code = c
			break
		}
	}
	if code == "" {
		return nil, errors.New("failed to generate a unique pairing code")
	}

	pc := &PairingCode{
		Code:      code,
		Hostname:  hostname,
		ExpiresAt: time.Now().UTC().Add(r.pairingTTL),
		Used:      false,
	}
	r.codes[code] = pc
	if err := r.saveLocked(); err != nil {
		return nil, err
	}
	cp := *pc
	return &cp, nil
}

// ApprovePairing approves a pairing code and registers the device. Returns
// the new Device and a raw (unhashed) device token.
func (r *FileRegistry) ApprovePairing(ctx context.Context, code string) (*Device, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	pc, ok := r.codes[code]
	if !ok {
		return nil, "", ErrPairingCodeNotFound
	}
	if pc.Used {
		return nil, "", ErrPairingCodeUsed
	}
	if time.Now().UTC().After(pc.ExpiresAt) {
		return nil, "", ErrPairingCodeExpired
	}

	token, err := generateDeviceToken()
	if err != nil {
		return nil, "", err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(token), bcrypt.DefaultCost)
	if err != nil {
		return nil, "", fmt.Errorf("hash device token: %w", err)
	}

	id, err := generateUUIDv4()
	if err != nil {
		return nil, "", err
	}

	now := time.Now().UTC()
	rec := &deviceRecord{
		Device: Device{
			ID:           id,
			Label:        pc.Hostname,
			Online:       false,
			Capabilities: nil,
			LastSeen:     now,
			PairedAt:     now,
		},
		TokenHash: string(hash),
	}

	pc.Used = true
	r.devices[id] = rec

	if err := r.saveLocked(); err != nil {
		return nil, "", err
	}

	d := rec.Device
	return &d, token, nil
}

// RejectPairing rejects (invalidates) a pairing code.
func (r *FileRegistry) RejectPairing(ctx context.Context, code string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	pc, ok := r.codes[code]
	if !ok {
		return ErrPairingCodeNotFound
	}
	if pc.Used {
		return ErrPairingCodeUsed
	}
	pc.Used = true
	return r.saveLocked()
}

// Authenticate validates a device token and returns the device.
func (r *FileRegistry) Authenticate(ctx context.Context, token string) (*Device, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, rec := range r.devices {
		if bcrypt.CompareHashAndPassword([]byte(rec.TokenHash), []byte(token)) == nil {
			d := rec.Device
			return &d, nil
		}
	}
	return nil, ErrAuthFailed
}

// Get returns a device by ID.
func (r *FileRegistry) Get(ctx context.Context, id string) (*Device, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	rec, ok := r.devices[id]
	if !ok {
		return nil, ErrDeviceNotFound
	}
	d := rec.Device
	return &d, nil
}

// List returns all paired devices.
func (r *FileRegistry) List(ctx context.Context) ([]*Device, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]*Device, 0, len(r.devices))
	for _, rec := range r.devices {
		d := rec.Device
		out = append(out, &d)
	}
	return out, nil
}

// SetOnline marks a device as online/offline and updates LastSeen.
func (r *FileRegistry) SetOnline(ctx context.Context, id string, online bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	rec, ok := r.devices[id]
	if !ok {
		return ErrDeviceNotFound
	}
	rec.Online = online
	rec.LastSeen = time.Now().UTC()
	return r.saveLocked()
}

// UpdateCapabilities updates the capability list for a device.
func (r *FileRegistry) UpdateCapabilities(ctx context.Context, id string, caps []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	rec, ok := r.devices[id]
	if !ok {
		return ErrDeviceNotFound
	}
	rec.Capabilities = caps
	return r.saveLocked()
}

// Revoke removes a device from the registry (operator-initiated).
func (r *FileRegistry) Revoke(ctx context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.devices[id]; !ok {
		return ErrDeviceNotFound
	}
	delete(r.devices, id)
	return r.saveLocked()
}

// PendingPairingCodes returns all non-expired, unused pairing codes.
func (r *FileRegistry) PendingPairingCodes(ctx context.Context) ([]*PairingCode, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now().UTC()
	var out []*PairingCode
	for _, pc := range r.codes {
		if pc.Used || now.After(pc.ExpiresAt) {
			continue
		}
		cp := *pc
		out = append(out, &cp)
	}
	return out, nil
}

var _ DeviceRegistry = (*FileRegistry)(nil)

func generateDeviceToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate device token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func generateUUIDv4() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate device id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
