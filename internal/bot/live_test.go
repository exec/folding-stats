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
		base = "https://foldingstats.org"
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
	alerts, err := OpenAlerts(dir + "/alerts.json")
	if err != nil {
		t.Fatal(err)
	}
	return &Bot{
		api:    c,
		links:  links,
		alerts: alerts,
		log:    slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
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

	// The composed answers, which used to come back as MCP prose in a code block.
	t.Run("rivals", func(t *testing.T) {
		e, err := b.cmdRivals(ctx, "teams", "51")
		nonEmpty(t, "rivals", e, err)
		mustNotBeACodeBlock(t, "rivals", e)
		// Whichever side has neighbours, at least one has to be there.
		if len(e.Fields) == 0 && e.Description == "" {
			t.Error("rivals returned nothing at all")
		}
		mustCaveat(t, "rivals", e, "7-day average")
	})

	t.Run("compare", func(t *testing.T) {
		e, err := b.cmdCompare(ctx, "teams", "51", "32")
		nonEmpty(t, "compare", e, err)
		mustNotBeACodeBlock(t, "compare", e)
		if len(e.Fields) < 3 {
			t.Errorf("compare should show both sides and the gap, got %d fields", len(e.Fields))
		}
		if e.Description == "" {
			t.Error("compare drew no conclusion")
		}
	})

	t.Run("movers", func(t *testing.T) {
		for _, dir := range []string{"", "up", "down"} {
			e, err := b.cmdMovers(ctx, "teams", dir)
			nonEmpty(t, "movers "+dir, e, err)
			mustNotBeACodeBlock(t, "movers", e)
			want := 2
			if dir != "" {
				want = 1
			}
			if len(e.Fields) != want {
				t.Errorf("movers %q: %d fields, want %d", dir, len(e.Fields), want)
			}
		}
	})

	t.Run("goal", func(t *testing.T) {
		e, err := b.cmdGoal(ctx, "teams", "51", 5)
		nonEmpty(t, "goal", e, err)
		mustNotBeACodeBlock(t, "goal", e)
		if e.Description == "" {
			t.Error("goal did not say what it would take")
		}
		// Already past the target is a different, shorter answer.
		if e2, err := b.cmdGoal(ctx, "teams", "51", 100000); err != nil {
			t.Fatal(err)
		} else if !strings.Contains(e2.Title, "already") {
			t.Errorf("goal past the target read as %q", e2.Title)
		}
	})
}

// mustNotBeACodeBlock is the regression this set exists for.
//
// These four answers used to be rendered by asking the MCP endpoint and wrapping its
// reply in triple backticks. That text is written for a model — fixed-width, wide, and
// full of prose — and inside an embed it does not wrap, scrolls sideways on a phone,
// and reads as debug output beside every other command.
func mustNotBeACodeBlock(t *testing.T, name string, e *discordgo.MessageEmbed) {
	t.Helper()
	if strings.Contains(e.Description, "```") {
		t.Errorf("%s is still a code block:\n%s", name, e.Description)
	}
}

// mustCaveat keeps the assumption attached to the figure.
//
// The reason these went through MCP in the first place was that those tools state what
// a projection assumes. Composing the answer here means carrying that sentence too —
// a caveat that travels separately from the number is one nobody repeats.
func mustCaveat(t *testing.T, name string, e *discordgo.MessageEmbed, want string) {
	t.Helper()
	var text string
	if e.Footer != nil {
		text = e.Footer.Text
	}
	if !strings.Contains(text+e.Description, want) {
		t.Errorf("%s lost its %q caveat: footer=%q", name, want, text)
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
		base = "https://foldingstats.org"
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

		checkOptions(t, c.Name, c.Options)
	}
}

// checkOptions walks a command's options and those of any subcommand.
//
// The flat version missed everything inside /alert, where the rules bite hardest: a
// required option after an optional one, or autocomplete alongside fixed choices,
// makes Discord reject the whole registration with a message that does not name the
// offending command — at start-up, in a log nobody is reading.
func checkOptions(t *testing.T, path string, opts []*discordgo.ApplicationCommandOption) {
	t.Helper()
	required := true
	for _, o := range opts {
		where := path + "." + o.Name
		if o.Name == "" || o.Description == "" {
			t.Errorf("%s: options need a name and a description", where)
		}
		if o.Name != strings.ToLower(o.Name) {
			t.Errorf("%s: option names must be lowercase", where)
		}
		if len(o.Description) > 100 {
			t.Errorf("%s: description over 100 chars", where)
		}
		sub := o.Type == discordgo.ApplicationCommandOptionSubCommand ||
			o.Type == discordgo.ApplicationCommandOptionSubCommandGroup
		if !sub {
			if o.Required && !required {
				t.Errorf("%s: required option %q follows an optional one", path, o.Name)
			}
			required = o.Required
		}
		if o.Autocomplete && len(o.Choices) > 0 {
			t.Errorf("%s: autocomplete and fixed choices are mutually exclusive", where)
		}
		if o.Autocomplete && o.Type != discordgo.ApplicationCommandOptionString &&
			o.Type != discordgo.ApplicationCommandOptionInteger &&
			o.Type != discordgo.ApplicationCommandOptionNumber {
			t.Errorf("%s: only string, integer and number options may autocomplete", where)
		}
		if sub {
			checkOptions(t, where, o.Options)
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

// TestAlertLifecycle drives the whole /alert surface against the live service.
//
// No Discord session, which is exactly the point: everything that can be wrong here —
// resolving a target, seeding it so it does not immediately fire, scoping, removal —
// happens before a message is ever sent. The session-shaped hole is announce(), which
// is a no-op without one.
func TestAlertLifecycle(t *testing.T) {
	b := liveBot(t)
	ctx := context.Background()

	i := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		GuildID:   "g1",
		ChannelID: "c1",
		User:      &discordgo.User{ID: "u1"},
	}}
	data := &discordgo.ApplicationCommandInteractionData{}
	opt := func(name string, v any) *discordgo.ApplicationCommandInteractionDataOption {
		t := discordgo.ApplicationCommandOptionString
		if _, isNum := v.(float64); isNum {
			t = discordgo.ApplicationCommandOptionInteger
		}
		return &discordgo.ApplicationCommandInteractionDataOption{Name: name, Type: t, Value: v}
	}

	// Add one against a donor that exists in every corpus.
	add := map[string]*discordgo.ApplicationCommandInteractionDataOption{
		"type":      opt("type", string(AlertMilestone)),
		"target":    opt("target", "d:Anonymous"),
		"threshold": opt("threshold", float64(0)),
	}
	e, err := b.alertAdd(ctx, i, data, add)
	if err != nil {
		t.Fatal(err)
	}
	if e.Color == colourBad {
		t.Fatalf("add refused: %s", e.Description)
	}
	if b.alerts.Count() != 1 {
		t.Fatalf("alert not stored: %d", b.alerts.Count())
	}

	stored := b.alerts.InScope("g1", "c1")[0]
	if stored.Kind != "donor" || stored.Target != "Anonymous" {
		t.Errorf("target resolved wrong: %+v", stored)
	}
	// Seeded from a live reading, so the first evaluation has something to compare to.
	if stored.Seen.Milestone == 0 || stored.Seen.PointsTotal == 0 {
		t.Errorf("alert was not seeded from a live reading: %+v", stored.Seen)
	}
	// And therefore does not fire on the very next evaluation.
	live, err := b.reading(ctx, stored.Kind, stored.Target)
	if err != nil {
		t.Fatal(err)
	}
	if fire, headline, _, _ := evaluate(stored, live, time.Now().UTC()); fire {
		t.Errorf("a freshly added alert fired immediately: %q", headline)
	}

	// A second identical one is refused rather than doubling every message.
	if e, _ := b.alertAdd(ctx, i, data, add); e.Color != colourBad {
		t.Error("duplicate alert was accepted")
	}

	// Listing shows it; another guild's listing does not.
	if e, err := b.alertList(i); err != nil {
		t.Fatal(err)
	} else if !strings.Contains(e.Description, stored.ID) {
		t.Errorf("list does not mention the alert: %q", e.Description)
	}
	other := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		GuildID: "g2", ChannelID: "c2", User: &discordgo.User{ID: "u2"},
	}}
	if e, _ := b.alertList(other); !strings.Contains(e.Title, "No alerts") {
		t.Errorf("another guild can see this one's alerts: %q", e.Description)
	}
	// And cannot remove it.
	if e, _ := b.alertRemove(other, stored.ID); e.Color != colourBad {
		t.Error("another guild removed this one's alert")
	}
	if b.alerts.Count() != 1 {
		t.Fatal("cross-guild removal succeeded")
	}

	if e, err := b.alertRemove(i, stored.ID); err != nil {
		t.Fatal(err)
	} else if e.Color == colourBad {
		t.Errorf("owner could not remove: %s", e.Description)
	}
	if b.alerts.Count() != 0 {
		t.Error("removal left it behind")
	}
}

// A target nobody can look up must be refused at setup rather than silently never
// firing — the failure mode alerts are worst at is doing nothing quietly.
func TestAlertRejectsAnUnknownTarget(t *testing.T) {
	b := liveBot(t)
	i := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		GuildID: "g1", ChannelID: "c1", User: &discordgo.User{ID: "u1"},
	}}
	e, err := b.alertAdd(context.Background(), i, &discordgo.ApplicationCommandInteractionData{},
		map[string]*discordgo.ApplicationCommandInteractionDataOption{
			"type":   {Name: "type", Type: discordgo.ApplicationCommandOptionString, Value: string(AlertIdle)},
			"target": {Name: "target", Type: discordgo.ApplicationCommandOptionString, Value: "NoSuchDonorXYZ123"},
		})
	if err != nil {
		t.Fatal(err)
	}
	if e.Color != colourBad {
		t.Errorf("accepted an unknown target: %s", e.Description)
	}
	if b.alerts.Count() != 0 {
		t.Error("stored an alert for a target that does not exist")
	}
}
