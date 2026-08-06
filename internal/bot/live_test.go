package bot

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// These exercise every command's data path against a running service.
//
// A Discord bot is almost impossible to test through Discord — you need a token, a
// guild, and a human to type. But the token only decides where the answer is
// delivered; everything that can actually be wrong happens before that. So the
// handlers are written to take no session, and this drives them directly.
//
// Skipped unless FOLDING_API points at something reachable, so `go test ./...` stays
// green on a machine with no network.
func liveBot(t *testing.T) *Bot {
	t.Helper()
	base := os.Getenv("FOLDING_API")
	if base == "" {
		base = "https://folding.exec.codes"
	}
	c := NewClient(base)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, err := c.Get(ctx, "/v1/status", nil); err != nil {
		t.Skipf("no reachable service at %s: %v", base, err)
	}
	dir := t.TempDir()
	links, err := OpenLinks(dir + "/links.json")
	if err != nil {
		t.Fatal(err)
	}
	return &Bot{
		api:    c,
		links:  links,
		log:    slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		mcpURL: base + "/mcp",
	}
}

// nonEmpty asserts an embed a person would find useful rather than merely valid.
func nonEmpty(t *testing.T, name string, e *discordgo.MessageEmbed, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if e == nil {
		t.Fatalf("%s: nil embed", name)
	}
	if e.Title == "" && e.Description == "" {
		t.Errorf("%s: embed has neither title nor description", name)
	}
	if e.Color == colourBad {
		t.Errorf("%s: returned an error embed: %s", name, e.Description)
	}
	for _, f := range e.Fields {
		// Discord rejects the whole message if any field value is empty, and the
		// failure surfaces as "the application did not respond".
		if strings.TrimSpace(f.Value) == "" {
			t.Errorf("%s: field %q has an empty value", name, f.Name)
		}
		if len(f.Value) > 1024 {
			t.Errorf("%s: field %q is %d chars, over Discord's 1024 limit", name, f.Name, len(f.Value))
		}
	}
	if len(e.Description) > 4096 {
		t.Errorf("%s: description is %d chars, over Discord's 4096 limit", name, len(e.Description))
	}
	if len(e.Fields) > 25 {
		t.Errorf("%s: %d fields, over Discord's 25 limit", name, len(e.Fields))
	}
}

func TestLiveCommands(t *testing.T) {
	b := liveBot(t)
	ctx := context.Background()

	t.Run("status", func(t *testing.T) {
		e, err := b.cmdStatus(ctx)
		nonEmpty(t, "status", e, err)
	})

	t.Run("donor", func(t *testing.T) {
		e, err := b.cmdDonor(ctx, "Anonymous")
		nonEmpty(t, "donor", e, err)
		// Anonymous is the canonical shared name; the warning is the point.
		if !strings.Contains(e.Description, "shared by many") {
			t.Errorf("Anonymous did not carry the shared-name warning: %q", e.Description)
		}
	})

	t.Run("team by id", func(t *testing.T) {
		e, err := b.cmdTeam(ctx, "51")
		nonEmpty(t, "team by id", e, err)
	})

	t.Run("team by name", func(t *testing.T) {
		e, err := b.cmdTeam(ctx, "Alliance")
		nonEmpty(t, "team by name", e, err)
	})

	t.Run("top teams", func(t *testing.T) {
		e, err := b.cmdTop(ctx, "teams", "per_day", 10)
		nonEmpty(t, "top teams", e, err)
	})

	t.Run("top donors", func(t *testing.T) {
		e, err := b.cmdTop(ctx, "donors", "lifetime", 10)
		nonEmpty(t, "top donors", e, err)
	})

	for _, tc := range []struct {
		name string
		tool string
		args map[string]any
	}{
		{"rivals", "rivals", map[string]any{"kind": "teams", "who": "51"}},
		{"compare", "compare", map[string]any{"kind": "teams", "a": "51", "b": "32"}},
		{"movers", "movers", map[string]any{"kind": "teams", "limit": 5}},
		{"goal", "what_would_it_take", map[string]any{"kind": "teams", "who": "51", "target_rank": 5}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, err := b.mcpEmbed(ctx, tc.tool, tc.args, tc.name, "")
			nonEmpty(t, tc.name, e, err)
			// The MCP tools append their own "Data as of" line; the embed footer
			// already says it, and printing it twice reads like a bug.
			if strings.Contains(e.Description, "Data as of") {
				t.Errorf("%s: duplicate freshness line left in the body", tc.name)
			}
			if e.Footer == nil || !strings.Contains(e.Footer.Text, "data from") {
				t.Errorf("%s: no freshness footer", tc.name)
			}
		})
	}
}

// TestMissesSuggestRatherThanDeadEnd is the difference between a bot people keep and
// one they give up on: donor names are case-sensitive and unguessable, so the reply to
// a near-miss has to carry the way forward.
func TestMissesSuggestRatherThanDeadEnd(t *testing.T) {
	b := liveBot(t)
	e, err := b.cmdDonor(context.Background(), "anonymou")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.Description, "Did you mean") {
		t.Errorf("a near miss gave no suggestions: %q", e.Description)
	}
}

// TestLinkRoundTrip covers the one piece of state the bot owns.
func TestLinkRoundTrip(t *testing.T) {
	b := liveBot(t)
	ctx := context.Background()
	i := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		User: &discordgo.User{ID: "u1"},
	}}

	// Unlinked.
	e, err := b.cmdMe(ctx, i)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.Description, "/link") {
		t.Errorf("unlinked /me did not point at /link: %q", e.Description)
	}

	// A name that does not exist must not be stored, or /me breaks later with no clue.
	if _, err := b.cmdLink(ctx, i, "NoSuchDonorXYZ"); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.links.Get("u1"); ok {
		t.Error("stored a link to a donor the service does not have")
	}

	// A real one sticks and /me answers with it.
	if _, err := b.cmdLink(ctx, i, "Anonymous"); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.links.Get("u1"); got != "Anonymous" {
		t.Fatalf("link = %q, want Anonymous", got)
	}
	e, err = b.cmdMe(ctx, i)
	nonEmpty(t, "me", e, err)
	if e.Title != "Anonymous" {
		t.Errorf("/me title = %q", e.Title)
	}

	// And it survives a reopen, which is the only reason it is on disk.
	reopened, err := OpenLinks(b.links.path)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := reopened.Get("u1"); got != "Anonymous" {
		t.Errorf("after reopen link = %q, want Anonymous", got)
	}

	if _, err := b.cmdUnlink(i); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.links.Get("u1"); ok {
		t.Error("unlink left the binding in place")
	}
}

// TestCacheHoldsForOneSnapshot proves the bot is the well-behaved consumer the API
// documentation asks for: repeat commands must not become repeat requests.
func TestCacheHoldsForOneSnapshot(t *testing.T) {
	base := os.Getenv("FOLDING_API")
	if base == "" {
		base = "https://folding.exec.codes"
	}
	var hits int
	c := NewClient(base)
	orig := c.HTTP.Transport
	c.HTTP.Transport = countingRT{orig, &hits}

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, err := c.Get(ctx, "/v1/summary", new(Summary)); err != nil {
			t.Skipf("no reachable service: %v", err)
		}
	}
	if hits != 1 {
		t.Errorf("five identical calls made %d requests, want 1", hits)
	}
}

type countingRT struct {
	inner http.RoundTripper
	n     *int
}

func (c countingRT) RoundTrip(r *http.Request) (*http.Response, error) {
	*c.n++
	return c.inner.RoundTrip(r)
}

// TestCommandsAreValidForDiscord catches shape errors that otherwise surface only as a
// rejected registration at startup.
func TestCommandsAreValidForDiscord(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range Commands() {
		if c.Name == "" || c.Description == "" {
			t.Errorf("%+v: name and description are both required", c)
		}
		if c.Name != strings.ToLower(c.Name) {
			t.Errorf("%s: command names must be lowercase", c.Name)
		}
		if len(c.Description) > 100 {
			t.Errorf("%s: description is %d chars, over 100", c.Name, len(c.Description))
		}
		if seen[c.Name] {
			t.Errorf("%s: duplicate command", c.Name)
		}
		seen[c.Name] = true

		required := true
		for _, o := range c.Options {
			if len(o.Description) > 100 {
				t.Errorf("%s.%s: description over 100 chars", c.Name, o.Name)
			}
			// Discord rejects the command outright if an optional option precedes a
			// required one, and the message does not say which command.
			if o.Required && !required {
				t.Errorf("%s: required option %q follows an optional one", c.Name, o.Name)
			}
			required = o.Required
			if o.Autocomplete && len(o.Choices) > 0 {
				t.Errorf("%s.%s: autocomplete and fixed choices are mutually exclusive", c.Name, o.Name)
			}
		}
	}
}

// TestCommandsAreInstallableAnywhere pins the user-app configuration.
//
// A command with no integration_types defaults to guild-install only, and the failure
// is silent in the worst way: the app offers user installation, the install succeeds,
// and then the user has no commands and nothing says why.
func TestCommandsAreInstallableAnywhere(t *testing.T) {
	for _, c := range Commands() {
		if c.IntegrationTypes == nil || len(*c.IntegrationTypes) != 2 {
			t.Errorf("%s: expected both guild and user install", c.Name)
			continue
		}
		var user, guild bool
		for _, it := range *c.IntegrationTypes {
			user = user || it == discordgo.ApplicationIntegrationUserInstall
			guild = guild || it == discordgo.ApplicationIntegrationGuildInstall
		}
		if !user || !guild {
			t.Errorf("%s: integration types = %v", c.Name, *c.IntegrationTypes)
		}
		// Usable in a DM is the whole point of a user install; a command restricted
		// to guilds would install and then be unreachable.
		if c.Contexts == nil || len(*c.Contexts) != 3 {
			t.Errorf("%s: expected guild, bot DM and private channel contexts", c.Name)
		}
	}
}

// TestUserIDResolvesOutsideAGuild covers the context a user install mostly runs in.
//
// In a guild the invoker is Interaction.Member.User; in a DM, Member is nil and the
// invoker is Interaction.User. Reading only the first would make every /me in a DM
// look like an unlinked account.
func TestUserIDResolvesOutsideAGuild(t *testing.T) {
	inGuild := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		Member: &discordgo.Member{User: &discordgo.User{ID: "guild-user"}},
	}}
	inDM := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		User: &discordgo.User{ID: "dm-user"},
	}}
	if got := userID(inGuild); got != "guild-user" {
		t.Errorf("in a guild userID = %q", got)
	}
	if got := userID(inDM); got != "dm-user" {
		t.Errorf("in a DM userID = %q", got)
	}
}
