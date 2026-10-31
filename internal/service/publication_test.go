package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIngestPublishesBeforeReadersResume(t *testing.T) {
	dir := t.TempDir()
	a := seed(t, dir, twoCycles()[:1])
	svc, srv, db := newService(t, dir, a)
	defer db.Close()
	ctx := context.Background()
	if _, err := svc.Ingest(ctx); err != nil {
		t.Fatal(err)
	}
	seed(t, dir, twoCycles()[1:])
	// Hold the only writer connection so ingest pauses after Apply, before commit.
	tx, err := db.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	waits := db.DB().Stats().WaitCount
	ingested := make(chan error, 1)
	go func() { _, err := svc.Ingest(ctx); ingested <- err }()
	deadline := time.Now().Add(5 * time.Second)
	for db.DB().Stats().WaitCount == waits {
		if time.Now().After(deadline) {
			t.Fatal("ingest never reached the blocked commit")
		}
		time.Sleep(time.Millisecond)
	}
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/teams/32", nil))
		response <- rec
	}()
	var early *httptest.ResponseRecorder
	select {
	case early = <-response:
	case <-time.After(100 * time.Millisecond):
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := <-ingested; err != nil {
		t.Fatal(err)
	}
	if early != nil {
		t.Fatalf("served uncommitted state before publication: %s", early.Body.String())
	}
	var rec *httptest.ResponseRecorder
	select {
	case rec = <-response:
	case <-time.After(5 * time.Second):
		t.Fatal("reader stayed blocked after publication")
	}
	var result struct {
		Snapshot struct{ At time.Time } `json:"snapshot"`
		Data     struct {
			Points int64 `json:"points_total"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || !result.Snapshot.At.Equal(cyc(22)) || result.Data.Points != 1900 {
		t.Fatalf("reader resumed against mismatched state: %s", rec.Body.String())
	}
}

func TestCommitFailureWithdrawsPreviouslyPublishedState(t *testing.T) {
	dir := t.TempDir()
	a := seed(t, dir, twoCycles()[:1])
	svc, srv, db := newService(t, dir, a)
	if _, err := svc.Ingest(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	seed(t, dir, twoCycles()[1:])
	if _, err := svc.Ingest(context.Background()); err == nil || !strings.Contains(err.Error(), "restart required") {
		t.Fatalf("commit failure = %v, want restart required", err)
	}
	if srv.Current() != nil {
		t.Fatal("failed commit left a snapshot pointing at uncommitted totals")
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/teams/32", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed commit served HTTP %d, want 503 until restart", rec.Code)
	}
}
