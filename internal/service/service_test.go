package service

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"folding/internal/api"
	"folding/internal/feed"
	"folding/internal/store"
)

// world describes one cycle's upstream content.
type world struct {
	at    time.Time
	teams []string // "id\tname\tscore\twu"
	users []string // "name\tscore\twu\tteam"
}

func seed(t *testing.T, dir string, cycles []world) *feed.Archive {
	t.Helper()
	a := &feed.Archive{Root: filepath.Join(dir, "raw")}
	for _, c := range cycles {
		// Team feed publishes a minute before the user feed, as upstream does.
		teamBody := "ts\nteam\tteamname\tscore\twu\n" + strings.Join(c.teams, "\n") + "\n"
		userBody := "ts\nname\tscore\twu\tteam\n" + strings.Join(c.users, "\n") + "\n"
		if _, err := a.Put(feed.Teams, c.at.Add(-time.Minute), strings.NewReader(teamBody)); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Put(feed.Users, c.at, strings.NewReader(userBody)); err != nil {
			t.Fatal(err)
		}
	}
	return a
}

func newService(t *testing.T, dir string, a *feed.Archive) (*Service, *api.Server, *store.Store) {
	t.Helper()
	db, err := store.Open(filepath.Join(dir, "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	srv := api.NewServer()
	svc, err := New(a, db, srv, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	return svc, srv, db
}

func cyc(h int) time.Time { return time.Date(2026, 8, 2, h, 30, 0, 0, time.UTC) }

func twoCycles() []world {
	return []world{
		{cyc(21),
			[]string{"32\tocuk\t1000\t10", "51\talliance\t500\t5"},
			[]string{"DH\t600\t6\t32", "toTOW\t400\t4\t32", "DH\t300\t3\t51"},
		},
		{cyc(22),
			[]string{"32\tocuk\t1900\t19", "51\talliance\t700\t7"},
			[]string{"DH\t1000\t10\t32", "toTOW\t900\t9\t32", "DH\t400\t4\t51"},
		},
	}
}

func TestIngestAppliesArchivedCycles(t *testing.T) {
	dir := t.TempDir()
	a := seed(t, dir, twoCycles())
	svc, srv, db := newService(t, dir, a)
	defer db.Close()

	n, err := svc.Ingest(context.Background())
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if n != 2 {
		t.Fatalf("applied %d cycles, want 2", n)
	}
	snap := srv.Current()
	if snap == nil {
		t.Fatal("nothing published")
	}
	if !snap.At.Equal(cyc(22)) {
		t.Errorf("published at %v, want %v", snap.At, cyc(22))
	}
	if got := snap.Totals.PointsTotal; got != 2600 {
		t.Errorf("points_total = %d, want 2600", got)
	}
	// Team production must come from the team window, member from the member one.
	if got := snap.Totals.PointsLast24h; got != 1100 {
		t.Errorf("points_last_24h = %d, want 1100", got)
	}
}

func TestIngestIsIdempotent(t *testing.T) {
	// Ingest runs on every archived snapshot and on a timer; re-running must not
	// double-count or reapply anything.
	dir := t.TempDir()
	a := seed(t, dir, twoCycles())
	svc, srv, db := newService(t, dir, a)
	defer db.Close()

	if _, err := svc.Ingest(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := srv.Current().Totals

	n, err := svc.Ingest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("second Ingest applied %d cycles, want 0", n)
	}
	if got := srv.Current().Totals; got != before {
		t.Errorf("totals changed on re-ingest:\n got %+v\nwant %+v", got, before)
	}
}

func TestRestartResumesWithoutReapplying(t *testing.T) {
	// The restart path in full: a fresh process reloads identity from the database,
	// skips cycles already applied, and picks up only what is new. Getting this
	// wrong either loses history or double-counts it, and neither would error.
	dir := t.TempDir()
	a := seed(t, dir, twoCycles())

	svc, srv, db := newService(t, dir, a)
	if _, err := svc.Ingest(context.Background()); err != nil {
		t.Fatal(err)
	}
	firstTotals := srv.Current().Totals
	db.Close()

	// A third cycle arrives while we were down.
	if _, err := a.Put(feed.Teams, cyc(23).Add(-time.Minute),
		strings.NewReader("ts\nteam\tteamname\tscore\twu\n32\tocuk\t2500\t25\n51\talliance\t900\t9\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Put(feed.Users, cyc(23),
		strings.NewReader("ts\nname\tscore\twu\tteam\nDH\t1400\t14\t32\ntoTOW\t1100\t11\t32\nDH\t500\t5\t51\n")); err != nil {
		t.Fatal(err)
	}

	svc2, srv2, db2 := newService(t, dir, a)
	defer db2.Close()
	n, err := svc2.Ingest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("applied %d cycles after restart, want only the new one", n)
	}

	snap := srv2.Current()
	if !snap.At.Equal(cyc(23)) {
		t.Errorf("published at %v, want %v", snap.At, cyc(23))
	}
	if got := snap.Totals.PointsTotal; got != 3400 {
		t.Errorf("points_total = %d, want 3400", got)
	}
	// Cumulative totals must have advanced, not restarted.
	if snap.Totals.PointsTotal <= firstTotals.PointsTotal {
		t.Errorf("totals did not advance across restart: %d then %d",
			firstTotals.PointsTotal, snap.Totals.PointsTotal)
	}
}

func TestRestartPreservesDonorIdentity(t *testing.T) {
	// Identity slots are referenced by stored deltas. If a restart reassigned them,
	// history would silently attach to the wrong donor.
	dir := t.TempDir()
	a := seed(t, dir, twoCycles())

	svc, srv, db := newService(t, dir, a)
	if _, err := svc.Ingest(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := donorTotals(srv)
	db.Close()

	svc2, srv2, db2 := newService(t, dir, a)
	defer db2.Close()
	if _, err := svc2.Ingest(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Nothing new to apply, but the service must still publish so the API is live.
	if srv2.Current() == nil {
		t.Fatal("restart published nothing")
	}
	got := donorTotals(srv2)
	if len(got) != len(want) {
		t.Fatalf("donor count changed across restart: %d then %d", len(want), len(got))
	}
	for name, pts := range want {
		if got[name] != pts {
			t.Errorf("donor %q total = %d after restart, want %d", name, got[name], pts)
		}
	}
}

func TestRestartRestoresRatesNotJustTotals(t *testing.T) {
	// Cumulative totals come back from the identity tables, but the rate windows are
	// derived from stored deltas and must be replayed. Missing that leaves the
	// service serving correct totals with every rate at zero, recovering only after
	// a full week — and reporting no error the whole time.
	dir := t.TempDir()
	a := seed(t, dir, twoCycles())

	svc, srv, db := newService(t, dir, a)
	if _, err := svc.Ingest(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := srv.Current()
	wantTeam24h := before.Totals.PointsLast24h
	teamSlot, _ := before.State.TeamSlot(32)
	wantTeamRate := before.Teams.Last24h(teamSlot)
	dhID, _ := before.State.Names.Lookup("DH")
	dhSlot, _ := before.State.MemberSlot(dhID, 32)
	wantMemberRate := before.Members.Last24h(dhSlot)
	db.Close()

	if wantTeam24h == 0 || wantTeamRate == 0 || wantMemberRate == 0 {
		t.Fatalf("fixture produced no rates to compare (%d/%d/%d)",
			wantTeam24h, wantTeamRate, wantMemberRate)
	}

	svc2, srv2, db2 := newService(t, dir, a)
	defer db2.Close()
	if _, err := svc2.Ingest(context.Background()); err != nil {
		t.Fatal(err)
	}
	after := srv2.Current()

	if got := after.Totals.PointsLast24h; got != wantTeam24h {
		t.Errorf("summary points_last_24h = %d after restart, want %d", got, wantTeam24h)
	}
	slot2, _ := after.State.TeamSlot(32)
	if got := after.Teams.Last24h(slot2); got != wantTeamRate {
		t.Errorf("team 32 last_24h = %d after restart, want %d", got, wantTeamRate)
	}
	id2, _ := after.State.Names.Lookup("DH")
	ms2, _ := after.State.MemberSlot(id2, 32)
	if got := after.Members.Last24h(ms2); got != wantMemberRate {
		t.Errorf("DH last_24h = %d after restart, want %d", got, wantMemberRate)
	}
}

func donorTotals(srv *api.Server) map[string]int64 {
	snap := srv.Current()
	out := map[string]int64{}
	for i, d := range snap.Ranks.Donors {
		_ = i
		out[snap.State.Names.Name(d.NameID)] = d.Score
	}
	return out
}

func TestCorruptSnapshotDoesNotBlockLaterCycles(t *testing.T) {
	// Upstream serving a truncated file must cost that hour, not everything after.
	dir := t.TempDir()
	a := seed(t, dir, twoCycles())

	// Overwrite one payload with garbage that will not decompress.
	snaps, err := a.List(feed.Users)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snaps[0].Path, []byte("not zstd at all"), 0o644); err != nil {
		t.Fatal(err)
	}

	svc, srv, db := newService(t, dir, a)
	defer db.Close()
	n, err := svc.Ingest(context.Background())
	if err != nil {
		t.Fatalf("Ingest returned an error for one bad snapshot: %v", err)
	}
	if n != 1 {
		t.Errorf("applied %d cycles, want 1 (the good one)", n)
	}
	if srv.Current() == nil {
		t.Fatal("nothing published despite a usable cycle")
	}
}

// TestConcurrentReadsDuringIngest is the race check. The API serves from a published
// snapshot while the ingest loop mutates the very state that snapshot points at, so
// this must be run with -race to be meaningful.
func TestConcurrentReadsDuringIngest(t *testing.T) {
	dir := t.TempDir()

	var cycles []world
	for h := 0; h < 12; h++ {
		score := 1000 + h*100
		cycles = append(cycles, world{
			at: cyc(h),
			teams: []string{
				fmt.Sprintf("32\tocuk\t%d\t%d", score, h+1),
				fmt.Sprintf("51\talliance\t%d\t%d", score/2, h+1),
			},
			users: []string{
				fmt.Sprintf("DH\t%d\t%d\t32", score/2, h+1),
				fmt.Sprintf("toTOW\t%d\t%d\t32", score/3, h+1),
				// A new donor appears each cycle, forcing the identity arrays and
				// metrics windows to grow while readers are active.
				fmt.Sprintf("newcomer%d\t%d\t1\t51", h, h*10+1),
			},
		})
	}
	a := seed(t, dir, cycles[:2])
	svc, srv, db := newService(t, dir, a)
	defer db.Close()

	if _, err := svc.Ingest(context.Background()); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(srv)
	defer ts.Close()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			paths := []string{"/v1/summary", "/v1/teams", "/v1/donors",
				"/v1/teams/32", "/v1/donors/DH", "/v1/teams/32/members"}
			for {
				select {
				case <-stop:
					return
				default:
				}
				for _, p := range paths {
					resp, err := http.Get(ts.URL + p)
					if err != nil {
						continue
					}
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
			}
		}()
	}

	// Ingest the remaining cycles while those readers hammer the API.
	for i := 2; i < len(cycles); i++ {
		c := cycles[i]
		teamBody := "ts\nteam\tteamname\tscore\twu\n" + strings.Join(c.teams, "\n") + "\n"
		userBody := "ts\nname\tscore\twu\tteam\n" + strings.Join(c.users, "\n") + "\n"
		if _, err := a.Put(feed.Teams, c.at.Add(-time.Minute), strings.NewReader(teamBody)); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Put(feed.Users, c.at, strings.NewReader(userBody)); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Ingest(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	close(stop)
	wg.Wait()

	if snap := srv.Current(); !snap.At.Equal(cyc(11)) {
		t.Errorf("final published snapshot at %v, want %v", snap.At, cyc(11))
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestRoutineDriftIsNotStale pins the grace period to measured upstream behaviour.
//
// Upstream's interval is 3606–3613s, never under an hour, and drifts ~10s later each
// cycle. With no grace, every cycle would spend minutes flagged stale — a false alarm
// every hour, which is worse than no flag at all.
func TestRoutineDriftIsNotStale(t *testing.T) {
	dir := t.TempDir()
	svc, srv, db := newService(t, dir, seed(t, dir, twoCycles()))
	defer db.Close()
	if _, err := svc.Ingest(context.Background()); err != nil {
		t.Fatal(err)
	}

	at := srv.Current().At
	cases := []struct {
		name  string
		age   time.Duration
		stale bool
	}{
		{"fresh", 0, false},
		{"drifted 13s past the hour", time.Hour + 13*time.Second, false},
		{"a full poll interval late", time.Hour + 10*time.Minute, false},
		{"upstream missed a publish", 2 * time.Hour, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// staleAt is the same expression Publish evaluates, against a clock we control.
			now := at.Add(c.age)
			got := now.After(at.Add(nominalInterval).Add(staleGrace))
			if got != c.stale {
				t.Errorf("stale at age %v = %v, want %v", c.age, got, c.stale)
			}
		})
	}
}
