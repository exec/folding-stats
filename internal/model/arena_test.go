package model

import (
	"fmt"
	"strings"
	"testing"
)

func TestArenaInternIsStable(t *testing.T) {
	a := NewNameArena(4)
	first := a.Intern("DH")
	if got := a.Intern("DH"); got != first {
		t.Errorf("Intern not stable: %d then %d", first, got)
	}
	if a.Len() != 1 {
		t.Errorf("Len = %d, want 1", a.Len())
	}
	if got := a.Name(first); got != "DH" {
		t.Errorf("Name = %q, want %q", got, "DH")
	}
}

func TestArenaHandlesPathologicalNames(t *testing.T) {
	// Real donor names contain tabs and newlines and may be empty-ish; the arena
	// must treat them as opaque bytes.
	names := []string{"", "\t", "\n", "/\ndy-Houston", "oslo\t60p", "\terrabyte",
		"84036980", strings.Repeat("x", 128)}
	a := NewNameArena(4)
	ids := make([]int32, len(names))
	for i, n := range names {
		ids[i] = a.Intern(n)
	}
	if a.Len() != len(names) {
		t.Fatalf("Len = %d, want %d", a.Len(), len(names))
	}
	for i, n := range names {
		if got := a.Name(ids[i]); got != n {
			t.Errorf("Name(%d) = %q, want %q", ids[i], got, n)
		}
		if id, ok := a.Lookup(n); !ok || id != ids[i] {
			t.Errorf("Lookup(%q) = %d,%v want %d,true", n, id, ok, ids[i])
		}
	}
}

func TestArenaGrowsWithoutLosingEntries(t *testing.T) {
	// Growth rehashes every entry; a bug there would silently alias two names.
	const n = 50_000
	a := NewNameArena(8)
	for i := 0; i < n; i++ {
		if got := a.Intern(fmt.Sprintf("donor_%d", i)); got != int32(i) {
			t.Fatalf("Intern(%d) = %d, want %d", i, got, i)
		}
	}
	if a.Len() != n {
		t.Fatalf("Len = %d, want %d", a.Len(), n)
	}
	for i := 0; i < n; i++ {
		want := fmt.Sprintf("donor_%d", i)
		if got := a.Name(int32(i)); got != want {
			t.Fatalf("Name(%d) = %q, want %q", i, got, want)
		}
	}
}

func TestArenaLookupMissing(t *testing.T) {
	a := NewNameArena(4)
	a.Intern("present")
	if _, ok := a.Lookup("absent"); ok {
		t.Error("Lookup found a name that was never interned")
	}
}

func BenchmarkArenaIntern(b *testing.B) {
	names := make([]string, 100_000)
	for i := range names {
		names[i] = fmt.Sprintf("donor_%d_%x", i, i*2654435761)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a := NewNameArena(len(names))
		for _, n := range names {
			a.Intern(n)
		}
	}
}
