package devices

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/CloudKeter/rc-mcp/internal/redisclient"
)

const (
	deviceKeyPrefix  = "rc-mcp:device:"
	pairingKeyPrefix = "rc-mcp:pairing:"
)

// RedisRegistry is a Redis-backed implementation of DeviceRegistry
// (docs/specs/backend.md Section 7, Section 19: "multi-replica"), so every
// server replica pointed at the same Redis instance sees the same paired
// devices and pairing codes -- unlike FileRegistry, which is local to one
// replica's filesystem.
//
// Pairing codes are stored with a Redis TTL matching their expiry, so an
// expired code disappears on its own rather than requiring an explicit
// sweep; devices have no TTL (they persist until Revoke).
type RedisRegistry struct {
	kv         redisclient.KVStore
	pairingTTL time.Duration
}

// NewRedisRegistry constructs a RedisRegistry using kv for storage.
// pairingTTL <= 0 uses DefaultPairingCodeTTL.
func NewRedisRegistry(kv redisclient.KVStore, pairingTTL time.Duration) *RedisRegistry {
	if pairingTTL <= 0 {
		pairingTTL = DefaultPairingCodeTTL
	}
	return &RedisRegistry{kv: kv, pairingTTL: pairingTTL}
}

func (r *RedisRegistry) CreatePairingCode(ctx context.Context, hostname string) (*PairingCode, error) {
	var code string
	for i := 0; i < 10; i++ {
		c, err := generatePairingCode()
		if err != nil {
			return nil, err
		}
		if _, err := r.kv.Get(ctx, pairingKeyPrefix+c); err == redisclient.ErrKeyNotFound {
			code = c
			break
		}
	}
	if code == "" {
		return nil, fmt.Errorf("devices: failed to generate a unique pairing code")
	}

	pc := &PairingCode{
		Code:      code,
		Hostname:  hostname,
		ExpiresAt: time.Now().UTC().Add(r.pairingTTL),
		Used:      false,
	}
	if err := r.putPairingCode(ctx, pc); err != nil {
		return nil, err
	}
	return pc, nil
}

func (r *RedisRegistry) putPairingCode(ctx context.Context, pc *PairingCode) error {
	data, err := json.Marshal(pc)
	if err != nil {
		return fmt.Errorf("devices: marshal pairing code: %w", err)
	}
	ttl := time.Until(pc.ExpiresAt)
	if ttl <= 0 {
		ttl = time.Second // already-expired code: let Redis reap it almost immediately
	}
	return r.kv.Set(ctx, pairingKeyPrefix+pc.Code, string(data), ttl)
}

func (r *RedisRegistry) getPairingCode(ctx context.Context, code string) (*PairingCode, error) {
	data, err := r.kv.Get(ctx, pairingKeyPrefix+code)
	if err == redisclient.ErrKeyNotFound {
		return nil, ErrPairingCodeNotFound
	}
	if err != nil {
		return nil, err
	}
	var pc PairingCode
	if err := json.Unmarshal([]byte(data), &pc); err != nil {
		return nil, fmt.Errorf("devices: unmarshal pairing code: %w", err)
	}
	return &pc, nil
}

func (r *RedisRegistry) ApprovePairing(ctx context.Context, code string) (*Device, string, error) {
	pc, err := r.getPairingCode(ctx, code)
	if err != nil {
		return nil, "", err
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
		return nil, "", fmt.Errorf("devices: hash device token: %w", err)
	}
	id, err := generateUUIDv4()
	if err != nil {
		return nil, "", err
	}

	now := time.Now().UTC()
	rec := &deviceRecord{
		Device: Device{
			ID:       id,
			Label:    pc.Hostname,
			Online:   false,
			LastSeen: now,
			PairedAt: now,
		},
		TokenHash: string(hash),
	}
	if err := r.putDevice(ctx, rec); err != nil {
		return nil, "", err
	}

	pc.Used = true
	if err := r.putPairingCode(ctx, pc); err != nil {
		return nil, "", err
	}

	d := rec.Device
	return &d, token, nil
}

func (r *RedisRegistry) RejectPairing(ctx context.Context, code string) error {
	pc, err := r.getPairingCode(ctx, code)
	if err != nil {
		return err
	}
	if pc.Used {
		return ErrPairingCodeUsed
	}
	pc.Used = true
	return r.putPairingCode(ctx, pc)
}

func (r *RedisRegistry) putDevice(ctx context.Context, rec *deviceRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("devices: marshal device: %w", err)
	}
	return r.kv.Set(ctx, deviceKeyPrefix+rec.ID, string(data), 0)
}

func (r *RedisRegistry) getDeviceRecord(ctx context.Context, id string) (*deviceRecord, error) {
	data, err := r.kv.Get(ctx, deviceKeyPrefix+id)
	if err == redisclient.ErrKeyNotFound {
		return nil, ErrDeviceNotFound
	}
	if err != nil {
		return nil, err
	}
	var rec deviceRecord
	if err := json.Unmarshal([]byte(data), &rec); err != nil {
		return nil, fmt.Errorf("devices: unmarshal device: %w", err)
	}
	return &rec, nil
}

func (r *RedisRegistry) Authenticate(ctx context.Context, token string) (*Device, error) {
	keys, err := r.kv.Keys(ctx, deviceKeyPrefix+"*")
	if err != nil {
		return nil, err
	}
	// bcrypt comparison isn't index-able; every device's hash must be
	// checked, same as FileRegistry -- acceptable at paired-device scale
	// (tens to hundreds, not millions).
	for _, key := range keys {
		data, err := r.kv.Get(ctx, key)
		if err != nil {
			continue // deleted between Keys() and Get(): skip
		}
		var rec deviceRecord
		if err := json.Unmarshal([]byte(data), &rec); err != nil {
			continue
		}
		if bcrypt.CompareHashAndPassword([]byte(rec.TokenHash), []byte(token)) == nil {
			d := rec.Device
			return &d, nil
		}
	}
	return nil, ErrAuthFailed
}

func (r *RedisRegistry) Get(ctx context.Context, id string) (*Device, error) {
	rec, err := r.getDeviceRecord(ctx, id)
	if err != nil {
		return nil, err
	}
	d := rec.Device
	return &d, nil
}

func (r *RedisRegistry) List(ctx context.Context) ([]*Device, error) {
	keys, err := r.kv.Keys(ctx, deviceKeyPrefix+"*")
	if err != nil {
		return nil, err
	}
	out := make([]*Device, 0, len(keys))
	for _, key := range keys {
		data, err := r.kv.Get(ctx, key)
		if err != nil {
			continue
		}
		var rec deviceRecord
		if err := json.Unmarshal([]byte(data), &rec); err != nil {
			continue
		}
		d := rec.Device
		out = append(out, &d)
	}
	return out, nil
}

func (r *RedisRegistry) SetOnline(ctx context.Context, id string, online bool) error {
	rec, err := r.getDeviceRecord(ctx, id)
	if err != nil {
		return err
	}
	rec.Online = online
	rec.LastSeen = time.Now().UTC()
	return r.putDevice(ctx, rec)
}

func (r *RedisRegistry) UpdateCapabilities(ctx context.Context, id string, caps []string) error {
	rec, err := r.getDeviceRecord(ctx, id)
	if err != nil {
		return err
	}
	rec.Capabilities = caps
	return r.putDevice(ctx, rec)
}

func (r *RedisRegistry) Revoke(ctx context.Context, id string) error {
	if _, err := r.getDeviceRecord(ctx, id); err != nil {
		return err
	}
	return r.kv.Del(ctx, deviceKeyPrefix+id)
}

func (r *RedisRegistry) PendingPairingCodes(ctx context.Context) ([]*PairingCode, error) {
	keys, err := r.kv.Keys(ctx, pairingKeyPrefix+"*")
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	var out []*PairingCode
	for _, key := range keys {
		data, err := r.kv.Get(ctx, key)
		if err != nil {
			continue
		}
		var pc PairingCode
		if err := json.Unmarshal([]byte(data), &pc); err != nil {
			continue
		}
		if pc.Used || now.After(pc.ExpiresAt) {
			continue
		}
		out = append(out, &pc)
	}
	return out, nil
}

var _ DeviceRegistry = (*RedisRegistry)(nil)
