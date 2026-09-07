package bot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// The payloads from the 2026-08-13 audit, verbatim. Donor and team names are chosen by
// the public and arrive unescaped, so every Discord surface that renders one has to
// treat it as data rather than as the markup it is trying to be.
const (
	// Closes the bold span the warning and alert descriptions put it inside, then
	// renders a clickable link the bot appears to have authored.
	boldEscape = "A**\n[V](https://x.io)\n**"
	// Closes the fence /top wraps its table in, then reopens one before the bot's own
	// closing delimiter so the message still looks well-formed.
	fenceEscape = "A```\n[V](https://x.io)\n```"
)

func TestMdEscNeutralisesTheBoldEscape(t *testing.T) {
	got := mdEsc(boldEscape)
	for _, bad := range []string{"**", "[V]", "](", "https://x.io)"} {
		if strings.Contains(got, bad) {
			t.Errorf("mdEsc left %q live in %q", bad, got)
		}
	}
	// The name must still be readable, not deleted.
	if !strings.Contains(got, "A") || !strings.Contains(got, "V") {
		t.Errorf("mdEsc destroyed the name: %q", got)
	}
}

func TestCodeSafeCannotCloseAFence(t *testing.T) {
	got := codeSafe(fenceEscape)
	if strings.Contains(got, "`") {
		t.Errorf("codeSafe left a backtick, which can close the block: %q", got)
	}
	if strings.ContainsAny(got, "\n\r\t") {
		t.Errorf("codeSafe left a line break, which breaks table alignment: %q", got)
	}
	// mdEsc would be the wrong tool here: inside a fence its backslashes are printed
	// literally rather than escaping anything.
	if strings.Contains(codeSafe("plain name"), "\\") {
		t.Error("codeSafe is escaping rather than sanitising")
	}
}

// The end the reader actually sees: whatever a name contains, the assembled embed must
// hold exactly one preformatted region.
func TestTopBodyKeepsOneFence(t *testing.T) {
	var sb strings.Builder
	for i, name := range []string{"honest", fenceEscape, "also honest"} {
		sb.WriteString(strings.TrimRight(
			padded(i+1, trunc(codeSafe(name), 28), "1.2M"), " ") + "\n")
	}
	e := TextEmbed("Top", "https://foldingstats.org/donors", sb.String(), Snapshot{})

	// Exactly two delimiters means the block opened once and closed once, so nothing
	// in it was ever rendered as markdown.
	if n := strings.Count(e.Description, "```"); n != 2 {
		t.Errorf("description has %d fence delimiters, want exactly 2:\n%s", n, e.Description)
	}
	// The link syntax survives as visible text, and that is the point rather than a
	// miss: inside a fence it is inert, so the reader sees the name the participant
	// actually chose instead of a mangled one. What matters is where it sits.
	open := strings.Index(e.Description, "```")
	close := strings.LastIndex(e.Description, "```")
	at := strings.Index(e.Description, "](https://x.io)")
	if at < 0 {
		t.Fatal("the name's text was destroyed rather than neutralised")
	}
	if at < open || at > close {
		t.Errorf("link syntax landed outside the fence, where it renders:\n%s", e.Description)
	}
}

// Truncation must not be able to manufacture a closing fence, so hostile input is
// checked either side of the 28-byte boundary the leaderboard truncates at.
func TestTruncationCannotRebuildAFence(t *testing.T) {
	for _, n := range []int{26, 27, 28, 29, 30} {
		name := strings.Repeat("a", n) + "```"
		if got := trunc(codeSafe(name), 28); strings.Contains(got, "`") {
			t.Errorf("length %d produced a backtick after truncation: %q", n, got)
		}
	}
}

func TestCommandNamesStayLiteral(t *testing.T) {
	for _, name := range []struct{ label, raw, escaped, clipped string }{
		{"plain", "plain name", "plain name", "plain name"},
		{"markdown", boldEscape, "A\\*\\*\n\\[V\\]\\(https://x.io\\)\n\\*\\*", "A\\*\\*\n\\[V\\]\\(https://x.io\\)\n\\*\\*"},
		{"clip boundary", strings.Repeat("é", 28) + "\\*tail", strings.Repeat("é", 28) + "\\\\\\*tail", strings.Repeat("é", 28) + "\\\\…"},
	} {
		t.Run(name.label, func(t *testing.T) {
			for _, tc := range []struct {
				label, command, kind string
				status               int
				saved                bool
				want                 string
			}{
				{"missing link", "link", "", http.StatusNotFound, false, "No donor named **" + name.escaped + "**. Names are case-sensitive and must match exactly — start typing and pick from the list."},
				{"saved link", "link", "", http.StatusOK, true, "You are linked to **" + name.escaped + "**. Run **/link** with a name to change it, or **/unlink** to clear it."},
				{"missing me", "me", "", http.StatusNotFound, true, "**" + name.escaped + "** is linked to your account but the service no longer has it. Use **/link** to set it again."},
				{"successful link", "link", "", http.StatusOK, false, "Linked to **" + name.escaped + "**. **/me** will show this from now on."},
				{"idle donor goal", "goal", "donors", http.StatusOK, false, "**" + name.clipped + "** is not producing right now, so at this rate the gap never closes. At **1M/day** it would take ~1 day."},
				{"idle team goal", "goal", "teams", http.StatusOK, false, "**" + name.clipped + "** is not producing right now, so at this rate the gap never closes. At **1M/day** it would take ~1 day."},
				{"API refusal", "link", "", http.StatusBadRequest, false, "No donor named " + name.escaped},
			} {
				t.Run(tc.label, func(t *testing.T) {
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(tc.status)
						var response any = map[string]any{"data": Donor{Name: name.raw, Rank: 2}}
						if tc.status != http.StatusOK {
							response = map[string]string{"error": "bad_request", "message": "No donor named " + name.raw}
						} else if r.URL.Path == "/v1/donors" || r.URL.Path == "/v1/teams" {
							response = map[string]any{"data": []Donor{{Name: "leader", Rank: 1, PointsTotal: 1_000_000}}}
						}
						if err := json.NewEncoder(w).Encode(response); err != nil {
							t.Error(err)
						}
					}))
					defer srv.Close()
					links, err := OpenLinks(t.TempDir() + "/links.json")
					if err != nil {
						t.Fatal(err)
					}
					if tc.saved {
						if err := links.Set("u1", name.raw); err != nil {
							t.Fatal(err)
						}
					}
					b := &Bot{api: NewClient(srv.URL), links: links}
					i := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{User: &discordgo.User{ID: "u1"}}}
					who := name.raw
					if tc.kind == "teams" {
						who = "1"
					}
					opts := map[string]string{"donor": name.raw, "kind": tc.kind, "who": who}
					if tc.saved {
						opts["donor"] = ""
					}
					e, err := b.run(context.Background(), i, tc.command,
						func(k string) string { return opts[k] }, func(string, int64) int64 { return 1 })
					if tc.status == http.StatusBadRequest {
						if err == nil {
							t.Fatal("API refusal returned no error")
						}
						e = ErrorEmbed(b.explain(err))
					} else if err != nil {
						t.Fatal(err)
					}
					if e.Description != tc.want {
						t.Errorf("description = %q, want %q", e.Description, tc.want)
					}
					if tc.label == "successful link" {
						if got, _ := links.Get("u1"); got != name.raw {
							t.Errorf("stored name = %q, want raw %q", got, name.raw)
						}
					}
				})
			}
		})
	}
}

func TestAlertErrorsKeepNamesLiteral(t *testing.T) {
	for _, name := range []struct{ label, raw, escaped string }{
		{"plain", "plain name", "plain name"},
		{"markdown", boldEscape, "A\\*\\*\n\\[V\\]\\(https://x.io\\)\n\\*\\*"},
	} {
		t.Run(name.label, func(t *testing.T) {
			for _, status := range []int{http.StatusOK, http.StatusBadRequest} {
				t.Run(http.StatusText(status), func(t *testing.T) {
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.WriteHeader(status)
						var response any = map[string]any{"data": Donor{Name: name.raw, Rank: 2}}
						if status != http.StatusOK {
							response = map[string]string{"error": "bad_request", "message": "No donor named " + name.raw}
						}
						if err := json.NewEncoder(w).Encode(response); err != nil {
							t.Error(err)
						}
					}))
					defer srv.Close()
					alerts, err := OpenAlerts(t.TempDir() + "/alerts.json")
					if err != nil {
						t.Fatal(err)
					}
					b := &Bot{api: NewClient(srv.URL), alerts: alerts}
					i := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
						User: &discordgo.User{ID: "u1"}, ChannelID: "c1",
					}}
					opts := map[string]*discordgo.ApplicationCommandInteractionDataOption{
						"type":   {Value: string(AlertMilestone)},
						"target": {Value: "d:" + name.raw},
					}
					add := func() (*discordgo.MessageEmbed, error) {
						return b.alertAdd(context.Background(), i, &discordgo.ApplicationCommandInteractionData{}, opts)
					}
					e, err := add()
					want := "No donor named " + name.escaped
					if status != http.StatusOK {
						if err == nil {
							t.Fatal("API refusal returned no error")
						}
						e = ErrorEmbed(b.explain(err))
					} else {
						if err != nil || e.Color != colourGood {
							t.Fatalf("adding alert: embed=%+v, err=%v", e, err)
						}
						e, err = add()
						if err != nil || e.Color != colourBad {
							t.Fatalf("duplicate alert: embed=%+v, err=%v", e, err)
						}
						want = "this channel already has that alert for " + name.escaped
					}
					if e.Description != want {
						t.Errorf("description = %q, want %q", e.Description, want)
					}
				})
			}
		})
	}
}

func padded(rank int, name, value string) string {
	return strings.Join([]string{itoa(rank) + ".", name, value}, " ")
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{byte('0' + i%10)}, b...)
	}
	return string(b)
}
