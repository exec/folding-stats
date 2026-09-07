package store

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestReadPoolPragmas(t *testing.T) {
	s := open(t)
	for i := 0; i < readPoolSize; i++ {
		conn, err := s.r.Conn(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		// Hold each connection so the next checkout must open another reader.
		defer conn.Close()
		for pragma, want := range map[string]int{
			"synchronous": 1, "temp_store": 2, "cache_size": -65536 / readPoolSize,
			"busy_timeout": 10000, "foreign_keys": 0,
		} {
			var got int
			if err := conn.QueryRowContext(t.Context(), "PRAGMA "+pragma).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Errorf("reader %d %s = %d, want %d", i, pragma, got, want)
			}
		}
	}
	var cacheSize int
	if err := s.w.QueryRow("PRAGMA cache_size").Scan(&cacheSize); err != nil || cacheSize != -65536 {
		t.Fatalf("writer cache_size = %d, err=%v", cacheSize, err)
	}
}

func historyFixture(t testing.TB) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	_, err = s.w.Exec(`
		WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i+1 FROM n WHERE i < 2159)
		INSERT INTO member_deltas SELECT 0, 1767225600+i*3600, i%7-1, i%3 FROM n;
		WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i+1 FROM n WHERE i < 729)
		INSERT INTO member_daily SELECT 0, 20454+i, i%7-1, i%3 FROM n;
		WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i+1 FROM n WHERE i < 23)
		INSERT INTO member_monthly SELECT 0, 24312+i, i%7-1, i%3 FROM n;
	`)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSingleMemberHistoryMatchesAggregate(t *testing.T) {
	s := historyFixture(t)
	from := time.Date(2026, 1, 3, 12, 0, 0, 0, time.UTC)
	to := from.AddDate(2, 0, 0)
	for _, g := range []Granularity{Hourly, Cycle, Daily, Weekly, Monthly} {
		got, err := s.MembersHistory(t.Context(), []int32{0}, from, to, g)
		if err != nil {
			t.Fatal(err)
		}
		want, err := s.MembersHistory(t.Context(), []int32{0, -1}, from, to, g)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("%s single member differs from aggregate: err=%v", g, err)
		}
	}
	got, err := s.MemberDays(t.Context(), []int32{0})
	if err != nil {
		t.Fatal(err)
	}
	want, err := s.MemberDays(t.Context(), []int32{0, -1})
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("single member days differ from aggregate: err=%v", err)
	}
}

func BenchmarkSingleMemberHistory(b *testing.B) {
	s := historyFixture(b)
	ctx := context.Background()
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(2, 0, 0)
	for _, g := range []Granularity{Hourly, Daily, Weekly, Monthly} {
		b.Run(string(g), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := s.MembersHistory(ctx, []int32{0}, from, to, g); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
	b.Run("days", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := s.MemberDays(ctx, []int32{0}); err != nil {
				b.Fatal(err)
			}
		}
	})
}
