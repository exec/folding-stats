package api

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Run with -race: metadata and the response's previous_at read live ingest state.
func TestSnapshotReadsDuringIngest(t *testing.T) {
	srv := fixture(t)
	snap := srv.Current()
	snap.Guard = new(sync.RWMutex)
	var workers sync.WaitGroup
	workers.Go(func() {
		for i := 0; i < 500; i++ {
			snap.Guard.Lock()
			snap.Teams.Push(snap.At.Add(time.Duration(i+1)*time.Hour), nil)
			snap.State.Teams[0].Score++
			snap.Guard.Unlock()
		}
	})
	for range 500 {
		if _, ok := srv.PageMeta("/teams/32"); !ok {
			t.Error("missing team metadata")
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/summary", nil))
		if rec.Code != http.StatusOK {
			t.Errorf("summary returned %d", rec.Code)
		}
	}
	workers.Wait()
}
