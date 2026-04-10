package rex

import (
	"sync"
	"testing"
)

func TestStore_SetGet(t *testing.T) {
	s := NewStore()
	if err := s.Set("auth.jwt", "token123"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	v, ok := s.Get("auth.jwt")
	if !ok || v != "token123" {
		t.Fatalf("Get: got %q, %v; want %q, true", v, ok, "token123")
	}
}

func TestStore_GetMissing(t *testing.T) {
	s := NewStore()
	_, ok := s.Get("nope")
	if ok {
		t.Fatal("Get missing key: want false")
	}
}

func TestStore_Del(t *testing.T) {
	s := NewStore()
	_ = s.Set("tenant.id", "team-a")
	if !s.Del("tenant.id") {
		t.Fatal("Del existing: want true")
	}
	if s.Del("tenant.id") {
		t.Fatal("Del missing: want false")
	}
	if s.Len() != 0 {
		t.Fatalf("Len after del: got %d, want 0", s.Len())
	}
}

func TestStore_All(t *testing.T) {
	s := NewStore()
	_ = s.Set("a", "1")
	_ = s.Set("b", "2")
	all := s.All()
	if len(all) != 2 || all["a"] != "1" || all["b"] != "2" {
		t.Fatalf("All: got %v", all)
	}

	// Mutating the returned map should not affect the store.
	all["a"] = "changed"
	v, _ := s.Get("a")
	if v != "1" {
		t.Fatal("All returned map is not a copy")
	}
}

func TestStore_Len(t *testing.T) {
	s := NewStore()
	if s.Len() != 0 {
		t.Fatal("empty store: want 0")
	}
	_ = s.Set("k", "v")
	if s.Len() != 1 {
		t.Fatal("one entry: want 1")
	}
}

func TestStore_Overwrite(t *testing.T) {
	s := NewStore()
	_ = s.Set("k", "v1")
	_ = s.Set("k", "v2")
	v, _ := s.Get("k")
	if v != "v2" {
		t.Fatalf("overwrite: got %q, want %q", v, "v2")
	}
	if s.Len() != 1 {
		t.Fatalf("overwrite len: got %d, want 1", s.Len())
	}
}

func TestValidateKey(t *testing.T) {
	tests := []struct {
		key     string
		wantErr bool
	}{
		{"auth.jwt", false},
		{"traceparent", false},
		{"tenant.id", false},
		{"x", false},
		{"", true},
		{"_start_ns", true},
		{"_anything", true},
		{"shared.something", true},
	}
	for _, tt := range tests {
		err := ValidateKey(tt.key)
		if (err != nil) != tt.wantErr {
			t.Errorf("ValidateKey(%q): err=%v, wantErr=%v", tt.key, err, tt.wantErr)
		}
	}
}

func TestStore_SetInvalidKey(t *testing.T) {
	s := NewStore()
	if err := s.Set("", "v"); err == nil {
		t.Fatal("empty key: want error")
	}
	if err := s.Set("_reserved", "v"); err == nil {
		t.Fatal("reserved key: want error")
	}
	if err := s.Set("shared.x", "v"); err == nil {
		t.Fatal("shared key: want error")
	}
}

func TestStore_Concurrent(t *testing.T) {
	s := NewStore()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.Set("k", "v")
			s.Get("k")
			s.All()
			s.Len()
			s.Del("k")
		}()
	}
	wg.Wait()
}
