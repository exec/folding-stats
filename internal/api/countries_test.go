package api

import "testing"

func TestCountryViewStacksTeams(t *testing.T) {
	s := fixture(t).Current()
	a, aok := s.State.TeamSlot(32)
	b, bok := s.State.TeamSlot(51)
	if !aok || !bok {
		t.Fatal("fixture teams missing")
	}
	wantA, wantB := s.teamView(a), s.teamView(b)

	c := s.countryView(countryDef{Code: "XX", Name: "Example", TeamIDs: []int32{32, 51}}, 10)
	if c.TeamsTotal != 2 || len(c.Teams) != 2 {
		t.Fatalf("teams = %d/%d, want 2/2", c.TeamsTotal, len(c.Teams))
	}
	if want := wantA.PointsTotal + wantB.PointsTotal; c.PointsTotal != want {
		t.Errorf("points = %d, want stacked total %d", c.PointsTotal, want)
	}
	if want := wantA.PointsPerDay24hAvg + wantB.PointsPerDay24hAvg; c.PointsPerDay24hAvg != want {
		t.Errorf("PPD = %d, want stacked rate %d", c.PointsPerDay24hAvg, want)
	}
}

// Dormant countries must survive the collection endpoint. They were filtered out of
// it originally, which is invisible until the map offers an all-time view — and then
// the countries that view exists for are the exact ones missing from it.
func TestCountriesIncludesDormantOnes(t *testing.T) {
	_, env := get(t, fixture(t), "/v1/countries")
	got := decode[[]Country](t, env.Data)
	if len(got) != len(countryDefs) {
		t.Fatalf("got %d countries, want all %d", len(got), len(countryDefs))
	}
	var dormant int
	for _, c := range got {
		if c.TeamsActive == 0 {
			dormant++
		}
	}
	if dormant == 0 {
		t.Fatal("no dormant country in the response; this test can no longer detect the filter")
	}
}

func TestCountryEndpointRejectsUnknownCode(t *testing.T) {
	rec, _ := get(t, fixture(t), "/v1/countries/XX")
	if rec.Code != 404 {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
