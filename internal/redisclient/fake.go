package redisclient

import (
	"context"
	"path"
	"sync"
	"time"
)

// fakeState is the shared, mutex-protected state behind one or more Fake
// handles. Multiple Fake values sharing the same *fakeState (via
// NewHandle) simulate multiple clients (e.g. two server replicas) talking
// to the same Redis instance -- data written or published through one
// handle is visible to every other handle sharing that state.
type fakeState struct {
	mu      sync.Mutex
	values  map[string]string
	expires map[string]time.Time     // key -> absolute expiry; absent = no TTL
	subs    map[string][]chan []byte // channel -> subscriber queues
}

// Fake is an in-memory KVStore/PubSub for tests that exercise Redis-backed
// implementations without a live Redis server. TTLs are honored via
// wall-clock expiry checked on access (Get/Keys), not a background sweep.
type Fake struct {
	state *fakeState
}

// NewFake constructs a Fake with its own independent state.
func NewFake() *Fake {
	return &Fake{state: &fakeState{
		values:  map[string]string{},
		expires: map[string]time.Time{},
		subs:    map[string][]chan []byte{},
	}}
}

// NewHandle returns a second Fake sharing f's underlying state, simulating
// another client connected to the same Redis instance (e.g. a second
// server replica in a cross-replica dispatch routing test).
func (f *Fake) NewHandle() *Fake {
	return &Fake{state: f.state}
}

func (f *Fake) expiredLocked(key string) bool {
	exp, ok := f.state.expires[key]
	return ok && time.Now().After(exp)
}

func (f *Fake) Get(ctx context.Context, key string) (string, error) {
	f.state.mu.Lock()
	defer f.state.mu.Unlock()
	if f.expiredLocked(key) {
		delete(f.state.values, key)
		delete(f.state.expires, key)
	}
	v, ok := f.state.values[key]
	if !ok {
		return "", ErrKeyNotFound
	}
	return v, nil
}

func (f *Fake) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	f.state.mu.Lock()
	defer f.state.mu.Unlock()
	f.state.values[key] = value
	if ttl > 0 {
		f.state.expires[key] = time.Now().Add(ttl)
	} else {
		delete(f.state.expires, key)
	}
	return nil
}

func (f *Fake) Expire(ctx context.Context, key string, ttl time.Duration) error {
	f.state.mu.Lock()
	defer f.state.mu.Unlock()
	if _, ok := f.state.values[key]; !ok {
		return nil
	}
	if ttl > 0 {
		f.state.expires[key] = time.Now().Add(ttl)
	} else {
		delete(f.state.expires, key)
	}
	return nil
}

func (f *Fake) Del(ctx context.Context, keys ...string) error {
	f.state.mu.Lock()
	defer f.state.mu.Unlock()
	for _, k := range keys {
		delete(f.state.values, k)
		delete(f.state.expires, k)
	}
	return nil
}

func (f *Fake) Keys(ctx context.Context, pattern string) ([]string, error) {
	f.state.mu.Lock()
	defer f.state.mu.Unlock()
	var out []string
	for k := range f.state.values {
		if f.expiredLocked(k) {
			continue
		}
		if ok, _ := path.Match(pattern, k); ok {
			out = append(out, k)
		}
	}
	return out, nil
}

func (f *Fake) Ping(ctx context.Context) error { return nil }
func (f *Fake) Close() error                   { return nil }

// Publish delivers payload to every handle currently subscribed to
// channel, across every Fake sharing this state.
func (f *Fake) Publish(ctx context.Context, channel string, payload []byte) error {
	f.state.mu.Lock()
	subs := append([]chan []byte(nil), f.state.subs[channel]...)
	f.state.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- payload:
		default:
			// Mirrors the real client's slow-consumer drop behavior.
		}
	}
	return nil
}

func (f *Fake) Subscribe(ctx context.Context, channel string) (Subscription, error) {
	ch := make(chan []byte, 64)
	f.state.mu.Lock()
	f.state.subs[channel] = append(f.state.subs[channel], ch)
	f.state.mu.Unlock()
	return &fakeSubscription{state: f.state, channel: channel, ch: ch}, nil
}

type fakeSubscription struct {
	state   *fakeState
	channel string
	ch      chan []byte
	once    sync.Once
}

func (s *fakeSubscription) Messages() <-chan []byte { return s.ch }

func (s *fakeSubscription) Close() error {
	s.once.Do(func() {
		s.state.mu.Lock()
		subs := s.state.subs[s.channel]
		for i, c := range subs {
			if c == s.ch {
				s.state.subs[s.channel] = append(subs[:i], subs[i+1:]...)
				break
			}
		}
		s.state.mu.Unlock()
		close(s.ch)
	})
	return nil
}

var (
	_ KVStore = (*Fake)(nil)
	_ PubSub  = (*Fake)(nil)
)
