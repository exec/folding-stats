package api

import (
	"sort"
	"strings"
	"testing"
)

func TestTopicViewStacksTeams(t *testing.T) {
	s := fixture(t).Current()
	a, aok := s.State.TeamSlot(32)
	b, bok := s.State.TeamSlot(51)
	if !aok || !bok {
		t.Fatal("fixture teams missing")
	}
	wantA, wantB := s.teamView(a), s.teamView(b)

	got := s.topicView(topicDef{Slug: "x", Name: "Example", TeamIDs: []int32{32, 51}}, 0)
	if got.TeamsTotal != 2 || len(got.Teams) != 2 {
		t.Fatalf("teams = %d/%d, want 2/2", got.TeamsTotal, len(got.Teams))
	}
	if want := wantA.PointsTotal + wantB.PointsTotal; got.PointsTotal != want {
		t.Errorf("points = %d, want stacked total %d", got.PointsTotal, want)
	}
}

func TestTopicEndpointRejectsUnknownSlug(t *testing.T) {
	rec, _ := get(t, fixture(t), "/v1/topics/not-a-topic")
	if rec.Code != 404 {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// Slugs reach the URL, so they must be unique and URL-safe, and every topic needs the
// description its card and its meta tag are built from.
func TestTopicDefsAreWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, def := range topicDefs {
		if seen[def.Slug] {
			t.Errorf("duplicate slug %q", def.Slug)
		}
		seen[def.Slug] = true
		if def.Slug != strings.ToLower(def.Slug) || strings.ContainsAny(def.Slug, " /?&#") {
			t.Errorf("slug %q is not URL-safe and lowercase", def.Slug)
		}
		if def.Name == "" || def.Description == "" {
			t.Errorf("%s: name and description are both required", def.Slug)
		}
		if len(def.TeamIDs) == 0 {
			t.Errorf("%s: no teams, so the card would advertise an empty page", def.Slug)
		}
		ids := map[int32]bool{}
		for _, id := range def.TeamIDs {
			if ids[id] {
				t.Errorf("%s: team %d listed twice, which would double its points", def.Slug, id)
			}
			ids[id] = true
		}
	}
}

// The whole reason topics are not countries: a team may belong to several. If this
// ever finds none, the many-per-team design has quietly become one-per-team.
func TestTeamsMayHoldSeveralTopics(t *testing.T) {
	count := map[int32][]string{}
	for _, def := range topicDefs {
		for _, id := range def.TeamIDs {
			count[id] = append(count[id], def.Slug)
		}
	}
	var multi int
	for _, slugs := range count {
		if len(slugs) > 1 {
			multi++
		}
	}
	if multi == 0 {
		t.Fatal("no team holds more than one topic")
	}
}

// The collection orders by how many teams gathered, not by output, so that a small
// community is not buried under a few overclockers with a lot of GPUs.
func TestTopicsAreOrderedByTeamCount(t *testing.T) {
	_, env := get(t, fixture(t), "/v1/topics")
	got := decode[[]Topic](t, env.Data)
	if len(got) != len(topicDefs) {
		t.Fatalf("got %d topics, want %d", len(got), len(topicDefs))
	}
	if !sort.SliceIsSorted(got, func(i, j int) bool { return got[i].TeamsTotal > got[j].TeamsTotal }) {
		t.Error("topics are not ordered by team count")
	}
}
