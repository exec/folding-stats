package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"folding/internal/model"
	"folding/internal/parse"
	"folding/internal/rank"
	"folding/internal/store"
)

func TestChangesFeedStopsAtPublishedSnapshot(t *testing.T) {
	srv := fixture(t)
	snap := srv.Current()
	future := model.NewState()
	ctx := context.Background()
	if err := snap.Store.LoadIdentity(ctx, future); err != nil {
		t.Fatal(err)
	}
	cycle := future.Apply(at(3),
		[]parse.TeamRow{tr(32, "overclockers", 2000), tr(51, "Alliance", 700)},
		[]parse.UserRow{u("DH", 1100, 32), u("toTOW", 900, 32), u("DH", 400, 51), u("solo", 300, 51)})
	// Ingest commits before it publishes. During this interval the database is newer
	// than the ranking used to build the response.
	if err := snap.Store.WriteCycle(ctx, future, cycle, store.CycleMeta{
		TeamSnapshotAt: at(3), UserSnapshotAt: at(3)}); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"teams", "members", "donors"} {
		path := fmt.Sprintf("/v1/changes?kind=%s&since=%d", kind, snap.At.Unix())
		rec, env := get(t, srv, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", path, rec.Code)
		}
		if got := decode[[]any](t, env.Data); len(got) != 0 || env.Page.TotalItems != 0 {
			t.Errorf("%s: future cycle leaked into published snapshot: %s", kind, rec.Body.String())
		}
	}

	// The upper bound includes the newest cycle once that cycle is published.
	snap.Members.Grow(len(future.Members))
	snap.Members.Push(at(3), cycle.MemberDeltas)
	snap.Teams.Grow(len(future.Teams))
	snap.Teams.Push(at(3), cycle.TeamDeltas)
	tbl := rank.Build(future, at(3), rank.DefaultConfig)
	tbl.BuildOrders(future, snap.Members, snap.Teams, nil, nil)
	srv.Publish(Build(future, snap.Members, snap.Teams, tbl, snap.Store, at(3), at(4), "new"))
	for _, kind := range []string{"teams", "members", "donors"} {
		_, env := get(t, srv, fmt.Sprintf("/v1/changes?kind=%s&since=%d", kind, at(2).Unix()))
		if got := decode[[]any](t, env.Data); len(got) != 1 || env.Page.TotalItems != 1 {
			t.Errorf("%s: newest published change missing: %+v", kind, env)
		}
	}
}
