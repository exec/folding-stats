package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCompareReturnsBothEntitiesAndProjection(t *testing.T) {
	_, env := get(t, fixture(t), "/v1/compare?kind=team&a=32&b=51")
	got := decode[Comparison](t, env.Data)
	if got.A.TeamID == nil || *got.A.TeamID != 32 || got.B.TeamID == nil || *got.B.TeamID != 51 {
		t.Fatalf("entities = %+v vs %+v", got.A, got.B)
	}
	if got.Leader != "a" || got.PointsGap != 1200 {
		t.Errorf("leader/gap = %q/%d, want a/1200", got.Leader, got.PointsGap)
	}
	if got.HorizonDays != overtakeHorizonDays {
		t.Errorf("horizon = %d", got.HorizonDays)
	}
}

func TestGoalIncludesMovingTargetInRequiredRate(t *testing.T) {
	_, env := get(t, fixture(t), "/v1/goals?kind=donor&who=solo&overtake=DH&by=2026-08-04")
	got := decode[Goal](t, env.Data)
	if got.TargetType != "overtake" || got.RequiredBy == nil || *got.RequiredBy <= got.Subject.PointsPerDay24hAvg {
		t.Fatalf("goal = %+v", got)
	}
	if got.Target.PointsPerDay24hAvg == 0 {
		t.Error("moving target lost its production rate")
	}
}

func TestGoalRejectsAmbiguousTarget(t *testing.T) {
	rec, _ := get(t, fixture(t), "/v1/goals?kind=team&who=51&target_rank=1&target_points=1000")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "exactly one") {
		t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
	}
}

func TestMoversAreSplitByDirection(t *testing.T) {
	_, env := get(t, rankChangeFixture(t), "/v1/movers?kind=team&within=10&limit=5")
	got := decode[Movers](t, env.Data)
	if len(got.Climbed) != 1 || got.Climbed[0].Change24h != 1 || *got.Climbed[0].TeamID != 32 {
		t.Errorf("climbed = %+v", got.Climbed)
	}
	if len(got.Fell) != 1 || got.Fell[0].Change24h != -1 || *got.Fell[0].TeamID != 51 {
		t.Errorf("fell = %+v", got.Fell)
	}
}

func TestBadgeIsEscapedCacheableSVG(t *testing.T) {
	srv := fixture(t)
	req := httptest.NewRequest(http.MethodGet, "/badge/donor/DH?metric=rank", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/svg+xml; charset=utf-8" {
		t.Fatalf("status/type = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if !strings.Contains(rec.Body.String(), "<svg") || !strings.Contains(rec.Body.String(), "rank") {
		t.Errorf("body = %s", rec.Body.String())
	}
	if rec.Header().Get("ETag") == "" || rec.Header().Get("Cache-Control") == "" {
		t.Error("badge missing cache headers")
	}

	// It is an image, not a JSON envelope.
	var v any
	if json.Unmarshal(rec.Body.Bytes(), &v) == nil {
		t.Error("badge unexpectedly decoded as JSON")
	}
}
