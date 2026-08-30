package session

import (
	"context"
	"sync"
	"testing"
)

func TestMemoryStore_CreateGetTouchDelete(t *testing.T) {
	store := NewMemoryStore(10)
	ctx := context.Background()

	s, err := store.Create(ctx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(s.ID) != 32 {
		t.Fatalf("session ID length = %d, want 32 (hex-encoded 128-bit)", len(s.ID))
	}

	got, err := store.Get(ctx, s.ID)
	if err != nil || got.ID != s.ID {
		t.Fatalf("Get = %v, %v", got, err)
	}

	if err := store.Touch(ctx, s.ID); err != nil {
		t.Fatalf("Touch: %v", err)
	}

	if err := store.Delete(ctx, s.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Get(ctx, s.ID); err != ErrNotFound {
		t.Fatalf("Get after delete = %v, want ErrNotFound", err)
	}

	select {
	case <-s.Ctx.Done():
	default:
		t.Fatal("session Ctx should be cancelled after Delete")
	}
}

func TestMemoryStore_GetUnknownID(t *testing.T) {
	store := NewMemoryStore(10)
	if _, err := store.Get(context.Background(), "unknown"); err != ErrNotFound {
		t.Fatalf("Get(unknown) = %v, want ErrNotFound", err)
	}
}

func TestMemoryStore_UniqueIDs(t *testing.T) {
	store := NewMemoryStore(10)
	ids := map[string]bool{}
	for i := 0; i < 100; i++ {
		s, err := store.Create(context.Background())
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if ids[s.ID] {
			t.Fatalf("duplicate session ID %s", s.ID)
		}
		ids[s.ID] = true
	}
}

func TestMemoryStore_Range(t *testing.T) {
	store := NewMemoryStore(10)
	for i := 0; i < 3; i++ {
		if _, err := store.Create(context.Background()); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	count := 0
	store.Range(func(*Session) bool {
		count++
		return true
	})
	if count != 3 {
		t.Fatalf("Range visited %d sessions, want 3", count)
	}
}

func TestMemoryStore_ConcurrentAccess(t *testing.T) {
	store := NewMemoryStore(10)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := store.Create(context.Background())
			if err != nil {
				t.Errorf("Create: %v", err)
				return
			}
			_ = store.Touch(context.Background(), s.ID)
			_, _ = store.Get(context.Background(), s.ID)
			_ = store.Delete(context.Background(), s.ID)
		}()
	}
	wg.Wait()
}
