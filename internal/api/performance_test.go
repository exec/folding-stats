package api

import (
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"folding/internal/metrics"
	"folding/internal/model"
	"folding/internal/parse"
	"folding/internal/rank"
)

// Reverse a ranking over one day so every candidate has a distinct movement.
func moverSnapshot(n int) *Snapshot {
	state := model.NewState()
	mw, tw := metrics.New(0), metrics.New(0)
	teams := make([]parse.TeamRow, n)
	users := make([]parse.UserRow, n)
	for cycle := 0; cycle < 2; cycle++ {
		for i := range teams {
			score := int64(n - i)
			if cycle == 1 {
				score = int64(n + i + 1)
			}
			teams[i] = tr(int32(i+1), fmt.Sprintf("team%d", i), score)
			users[i] = u(fmt.Sprintf("donor%d", i), score, int32(i+1))
		}
		when := at(0).Add(time.Duration(cycle) * 25 * time.Hour)
		c := state.Apply(when, teams, users)
		mw.Grow(len(state.Members))
		mw.Push(when, c.MemberDeltas)
		tw.Grow(len(state.Teams))
		tw.Push(when, c.TeamDeltas)
	}
	tbl := rank.Build(state, mw.At(), rank.DefaultConfig)
	tbl.BuildOrders(state, mw, tw, nil, nil)
	tbl.BuildChange24h(state, mw, tw)
	return Build(state, mw, tw, tbl, nil, mw.At(), mw.At().Add(time.Hour), "movers")
}

func BenchmarkMovers(b *testing.B) {
	snap := moverSnapshot(10000)
	srv := &Server{}
	for _, kind := range []string{"team", "donor"} {
		b.Run(kind+"/REST", func(b *testing.B) {
			r := httptest.NewRequest("GET", "/v1/movers?kind="+kind+"&within=10000&limit=25", nil)
			b.ReportAllocs()
			for b.Loop() {
				if _, _, err := srv.movers(snap, r); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(kind+"/MCP", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := snap.mcpMovers(kind+"s", "both", 10000, 25); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestMoversLimitBeforeBuildingViews(t *testing.T) {
	snap := moverSnapshot(40)
	srv := &Server{}
	for _, kind := range []string{"team", "donor"} {
		for _, direction := range []string{"both", "up", "down"} {
			r := httptest.NewRequest("GET", "/v1/movers?kind="+kind+"&within=100&limit=3&direction="+direction, nil)
			data, _, err := srv.movers(snap, r)
			if err != nil {
				t.Fatal(err)
			}
			got := data.(Movers)
			for _, side := range []struct {
				rows []Mover
				up   bool
			}{{got.Climbed, true}, {got.Fell, false}} {
				wantLen := 3
				if (side.up && direction == "down") || (!side.up && direction == "up") {
					wantLen = 0
				}
				if len(side.rows) != wantLen {
					t.Fatalf("%s %s: %d rows, want %d", kind, direction, len(side.rows), wantLen)
				}
				for i, row := range side.rows {
					pos := i + 1
					if !side.up {
						pos = 40 - i
					}
					want, err := snap.insightAtRank(kind, pos)
					if err != nil || !reflect.DeepEqual(row.InsightEntity, want) || row.Change24h != int32(41-2*pos) {
						t.Fatalf("%s %s row %d: %+v; want %+v", kind, direction, i, row, want)
					}
				}
			}
			text, err := snap.mcpMovers(kind+"s", direction, 100, 3)
			if err != nil || strings.Contains(text, "Climbed:") != (direction != "down") || strings.Contains(text, "Fell:") != (direction != "up") {
				t.Fatalf("%s %s MCP: %v\n%s", kind, direction, err, text)
			}
		}
	}
}

func TestRosterSortsByTheDisplayedDay(t *testing.T) {
	state := model.NewState()
	state.Apply(at(0), []parse.TeamRow{tr(1, "team", 40000)}, []parse.UserRow{
		u("lastweek", 20000, 1), u("yesterday", 10000, 1), u("today", 5000, 1),
	})
	mw, tw := metrics.New(3), metrics.New(1)
	mw.Push(at(0), nil)
	mw.Push(at(24), []model.Delta{{ID: 0, DScore: 1000}})
	mw.Push(at(95), []model.Delta{{ID: 1, DScore: 50}})
	mw.Push(at(97), []model.Delta{{ID: 2, DScore: 10}})
	tbl := rank.Build(state, mw.At(), rank.DefaultConfig)
	snap := Build(state, mw, tw, tbl, nil, mw.At(), at(98), "roster")
	srv := NewServer()
	srv.Publish(snap)
	for _, tc := range []struct{ key, first string }{
		{"per_day", "yesterday"}, {"points_per_day_24h_avg", "yesterday"},
		{"today", "today"}, {"points_today_utc", "today"}, {"daily", "today"},
	} {
		for _, filter := range []string{"", "&active_only=true"} {
			_, env := get(t, srv, "/v1/teams/1/members?sort="+tc.key+filter)
			rows := decode[[]Member](t, env.Data)
			if len(rows) != 3 || rows[0].Name != tc.first {
				t.Fatalf("%s%s: %+v", tc.key, filter, rows)
			}
		}
	}
}
