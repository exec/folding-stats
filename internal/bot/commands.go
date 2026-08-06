package bot

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// The command set answers the questions people actually ask in a folding channel.
//
// The shape follows the MCP tools rather than the REST routes, for the same reason:
// "am I catching up to them" is one question, and a bot that made you run three
// commands and do the arithmetic would be a worse leaderboard than the website.
//
// Every command taking a name uses autocomplete against /v1/search. Donor names are
// not unique, not guessable and frequently contain punctuation — requiring exact
// spelling would make the bot unusable for the names people most want to look up.
func Commands() []*discordgo.ApplicationCommand {
	kind := &discordgo.ApplicationCommandOption{
		Type:        discordgo.ApplicationCommandOptionString,
		Name:        "kind",
		Description: "teams or donors",
		Required:    true,
		Choices: []*discordgo.ApplicationCommandOptionChoice{
			{Name: "teams", Value: "teams"},
			{Name: "donors", Value: "donors"},
		},
	}
	sortOpt := &discordgo.ApplicationCommandOption{
		Type:        discordgo.ApplicationCommandOptionString,
		Name:        "sort",
		Description: "which column to rank by (default: lifetime points)",
		Choices: []*discordgo.ApplicationCommandOptionChoice{
			{Name: "lifetime points", Value: "lifetime"},
			{Name: "points per day", Value: "per_day"},
			{Name: "today", Value: "today"},
			{Name: "this week", Value: "this_week"},
			{Name: "this month", Value: "this_month"},
			{Name: "last 24 hours", Value: "last_24h"},
			{Name: "work units", Value: "wus"},
		},
	}
	donorOpt := func(req bool) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{
			Type:         discordgo.ApplicationCommandOptionString,
			Name:         "donor",
			Description:  "donor name — start typing and pick from the list",
			Required:     req,
			Autocomplete: true,
		}
	}
	teamOpt := func(req bool) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{
			Type:         discordgo.ApplicationCommandOptionString,
			Name:         "team",
			Description:  "team name or number — start typing and pick from the list",
			Required:     req,
			Autocomplete: true,
		}
	}

	return anywhere([]*discordgo.ApplicationCommand{
		{Name: "me", Description: "Your own folding stats (after /link)"},
		{
			Name: "link", Description: "Remember which donor name is yours, so /me works",
			Options: []*discordgo.ApplicationCommandOption{donorOpt(false)},
		},
		{Name: "unlink", Description: "Forget the donor name linked to your account"},
		{
			Name: "donor", Description: "Look up a folder",
			Options: []*discordgo.ApplicationCommandOption{donorOpt(true)},
		},
		{
			Name: "team", Description: "Look up a team",
			Options: []*discordgo.ApplicationCommandOption{teamOpt(true)},
		},
		{
			Name: "rivals", Description: "Who you are about to pass, and who is about to pass you",
			Options: []*discordgo.ApplicationCommandOption{kind, {
				Type: discordgo.ApplicationCommandOptionString, Name: "who",
				Description: "team or donor — start typing", Required: true, Autocomplete: true,
			}},
		},
		{
			Name: "compare", Description: "Two teams or two folders head to head",
			Options: []*discordgo.ApplicationCommandOption{kind,
				{Type: discordgo.ApplicationCommandOptionString, Name: "a", Description: "first", Required: true, Autocomplete: true},
				{Type: discordgo.ApplicationCommandOptionString, Name: "b", Description: "second", Required: true, Autocomplete: true},
			},
		},
		{
			Name: "top", Description: "Leaderboard",
			Options: []*discordgo.ApplicationCommandOption{kind, sortOpt, {
				Type: discordgo.ApplicationCommandOptionInteger, Name: "limit",
				Description: "how many (1-15, default 10)", MinValue: ptr(1.0), MaxValue: 15,
			}},
		},
		{
			Name: "movers", Description: "Biggest 24-hour rank movements",
			Options: []*discordgo.ApplicationCommandOption{kind, {
				Type: discordgo.ApplicationCommandOptionString, Name: "direction",
				Description: "climbing or falling",
				Choices: []*discordgo.ApplicationCommandOptionChoice{
					{Name: "climbing", Value: "up"}, {Name: "falling", Value: "down"},
				},
			}},
		},
		{
			Name: "goal", Description: "What it would take to reach a rank",
			Options: []*discordgo.ApplicationCommandOption{kind,
				{Type: discordgo.ApplicationCommandOptionString, Name: "who", Description: "team or donor", Required: true, Autocomplete: true},
				{Type: discordgo.ApplicationCommandOptionInteger, Name: "rank", Description: "the rank to reach", Required: true, MinValue: ptr(1.0)},
			},
		},
		{Name: "status", Description: "Project totals and how fresh the data is"},
		alertCommand(),
	})
}

func ptr[T any](v T) *T { return &v }

// anywhere makes a command available however the app was installed.
//
// Nothing this bot does needs a guild: every command is a read against a public API
// and the only state it keeps is keyed by Discord user id, which exists in a DM as
// readily as in a channel. So it is installable to a user as well as to a server, and
// usable in DMs, group DMs, and servers the app was never added to.
//
// Without these two fields a command defaults to guild-install only, which is the
// quiet failure here: the app advertises user installation, the install succeeds, and
// then no commands appear.
//
// One consequence worth knowing rather than working around: used in a server where
// only the *user* installed it, Discord makes the reply ephemeral — visible to the
// person who ran it and nobody else. That is right for /me and wrong for /top, which
// is why installing to the server as well is still worth doing.
func anywhere(cmds []*discordgo.ApplicationCommand) []*discordgo.ApplicationCommand {
	types := []discordgo.ApplicationIntegrationType{
		discordgo.ApplicationIntegrationGuildInstall,
		discordgo.ApplicationIntegrationUserInstall,
	}
	contexts := []discordgo.InteractionContextType{
		discordgo.InteractionContextGuild,
		discordgo.InteractionContextBotDM,
		discordgo.InteractionContextPrivateChannel,
	}
	for _, c := range cmds {
		c.IntegrationTypes = &types
		c.Contexts = &contexts
	}
	return cmds
}

/* ------------------------------------------------------------- dispatch --- */

func (b *Bot) handleCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.ApplicationCommandData()
	// The alert group reads nested options and replies ephemerally, so it owns its
	// whole interaction rather than sharing the flat path below.
	if data.Name == "alert" {
		b.handleAlert(s, i)
		return
	}
	opts := map[string]*discordgo.ApplicationCommandInteractionDataOption{}
	for _, o := range data.Options {
		opts[o.Name] = o
	}
	str := func(k string) string {
		if o, ok := opts[k]; ok {
			return o.StringValue()
		}
		return ""
	}
	num := func(k string, def int64) int64 {
		if o, ok := opts[k]; ok {
			return o.IntValue()
		}
		return def
	}

	// Deferred first. Discord gives three seconds, and while the API answers in under
	// a millisecond, a cold connection or a restarting service must not turn into
	// "the application did not respond" — an error the user cannot distinguish from
	// the bot being broken.
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	}); err != nil {
		b.log.Error("deferring", "cmd", data.Name, "err", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()

	embed, err := b.run(ctx, i, data.Name, str, num)
	if err != nil {
		embed = ErrorEmbed(b.explain(err))
	}
	if _, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Embeds: &[]*discordgo.MessageEmbed{embed},
	}); err != nil {
		b.log.Error("replying", "cmd", data.Name, "err", err)
	}
}

// explain turns a failure into something a person can act on.
//
// The service already writes good refusals — "no donor named X. Names are
// case-sensitive… use search" — so those are passed through verbatim. Only transport
// failures get rewritten, because "connection refused" tells a Discord user nothing.
func (b *Bot) explain(err error) string {
	var a *APIError
	if asAPIError(err, &a) {
		return a.Message
	}
	b.log.Error("command failed", "err", err)
	return "The statistics service is not answering right now. It publishes hourly, so this is usually brief."
}

func (b *Bot) run(ctx context.Context, i *discordgo.InteractionCreate, name string,
	str func(string) string, num func(string, int64) int64) (*discordgo.MessageEmbed, error) {

	switch name {
	case "status":
		return b.cmdStatus(ctx)
	case "me":
		return b.cmdMe(ctx, i)
	case "link":
		return b.cmdLink(ctx, i, str("donor"))
	case "unlink":
		return b.cmdUnlink(i)
	case "donor":
		return b.cmdDonor(ctx, str("donor"))
	case "team":
		return b.cmdTeam(ctx, str("team"))
	case "top":
		return b.cmdTop(ctx, str("kind"), str("sort"), int(num("limit", 10)))
	case "rivals":
		return b.mcpEmbed(ctx, "rivals", map[string]any{"kind": str("kind"), "who": str("who")},
			"Rivals", "")
	case "compare":
		return b.mcpEmbed(ctx, "compare", map[string]any{"kind": str("kind"), "a": str("a"), "b": str("b")},
			"Head to head", "")
	case "movers":
		args := map[string]any{"kind": str("kind"), "limit": 5}
		if d := str("direction"); d != "" {
			args["direction"] = d
		}
		return b.mcpEmbed(ctx, "movers", args, "Biggest 24-hour movements", "")
	case "goal":
		return b.mcpEmbed(ctx, "what_would_it_take",
			map[string]any{"kind": str("kind"), "who": str("who"), "target_rank": num("rank", 1)},
			"What it would take", "")
	}
	return nil, fmt.Errorf("unknown command %q", name)
}

/* -------------------------------------------------------------- commands --- */

func (b *Bot) cmdStatus(ctx context.Context) (*discordgo.MessageEmbed, error) {
	sum, snap, err := b.api.Summary(ctx)
	if err != nil {
		return nil, err
	}
	return &discordgo.MessageEmbed{
		Title: "Folding@home", URL: SiteURL, Color: colourNormal,
		Fields: []*discordgo.MessageEmbedField{
			field("Points", short(sum.PointsTotal), true),
			field("Work units", short(sum.WUsTotal), true),
			field("Last 24 hours", short(sum.PointsLast24h), true),
			field("Donors", fmt.Sprintf("%s producing\nof %s", n(sum.DonorsActive), n(sum.DonorsTotal)), true),
			field("Teams", fmt.Sprintf("%s producing\nof %s", n(sum.TeamsActive), n(sum.TeamsTotal)), true),
			field("Next update", fmt.Sprintf("<t:%d:R>", snap.NextExpectedAt.Unix()), true),
		},
		Footer: footer(snap),
	}, nil
}

func (b *Bot) cmdMe(ctx context.Context, i *discordgo.InteractionCreate) (*discordgo.MessageEmbed, error) {
	name, ok := b.links.Get(userID(i))
	if !ok {
		return ErrorEmbed("You have not linked a donor name yet. Use **/link** and pick yours from the list."), nil
	}
	d, snap, err := b.api.Donor(ctx, name)
	if err != nil {
		if NotFound(err) {
			return ErrorEmbed(fmt.Sprintf(
				"**%s** is linked to your account but the service no longer has it. Use **/link** to set it again.",
				name)), nil
		}
		return nil, err
	}
	return DonorEmbed(d, snap), nil
}

func (b *Bot) cmdLink(ctx context.Context, i *discordgo.InteractionCreate, donor string) (*discordgo.MessageEmbed, error) {
	if donor == "" {
		if cur, ok := b.links.Get(userID(i)); ok {
			return ErrorEmbed(fmt.Sprintf("You are linked to **%s**. Run **/link** with a name to change it, or **/unlink** to clear it.", cur)), nil
		}
		return ErrorEmbed("You are not linked to anyone. Run **/link** and start typing your folding name."), nil
	}
	// Confirm the name exists before storing it, or /me fails later with no clue why.
	d, snap, err := b.api.Donor(ctx, donor)
	if err != nil {
		if NotFound(err) {
			return ErrorEmbed(fmt.Sprintf(
				"No donor named **%s**. Names are case-sensitive and must match exactly — start typing and pick from the list.",
				donor)), nil
		}
		return nil, err
	}
	if err := b.links.Set(userID(i), d.Name); err != nil {
		return nil, err
	}
	e := DonorEmbed(d, snap)
	e.Description = strings.TrimSpace(fmt.Sprintf("Linked to **%s**. **/me** will show this from now on.\n\n%s", d.Name, e.Description))
	return e, nil
}

func (b *Bot) cmdUnlink(i *discordgo.InteractionCreate) (*discordgo.MessageEmbed, error) {
	if _, ok := b.links.Get(userID(i)); !ok {
		return ErrorEmbed("You were not linked to anyone."), nil
	}
	if err := b.links.Delete(userID(i)); err != nil {
		return nil, err
	}
	return ErrorEmbed("Unlinked."), nil
}

func (b *Bot) cmdDonor(ctx context.Context, name string) (*discordgo.MessageEmbed, error) {
	d, snap, err := b.api.Donor(ctx, name)
	if err != nil {
		if NotFound(err) {
			return b.suggest(ctx, name)
		}
		return nil, err
	}
	return DonorEmbed(d, snap), nil
}

func (b *Bot) cmdTeam(ctx context.Context, q string) (*discordgo.MessageEmbed, error) {
	// Autocomplete sends the id; a person typing sends a name. Accept both.
	if id, err := strconv.ParseInt(strings.TrimSpace(q), 10, 64); err == nil {
		t, snap, err := b.api.Team(ctx, id)
		if err == nil {
			return TeamEmbed(t, snap), nil
		}
		if !NotFound(err) {
			return nil, err
		}
	}
	res, _, err := b.api.Search(ctx, q, 5)
	if err != nil {
		return nil, err
	}
	if len(res.Teams) == 0 {
		return b.suggest(ctx, q)
	}
	t, snap, err := b.api.Team(ctx, res.Teams[0].TeamID)
	if err != nil {
		return nil, err
	}
	return TeamEmbed(t, snap), nil
}

// suggest answers a miss with the nearest matches rather than a dead end.
func (b *Bot) suggest(ctx context.Context, q string) (*discordgo.MessageEmbed, error) {
	res, _, err := b.api.Search(ctx, q, 6)
	if err != nil || (len(res.Donors) == 0 && len(res.Teams) == 0) {
		return ErrorEmbed(fmt.Sprintf("Nothing matches **%s**. Names are case-sensitive; try fewer characters.", q)), nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Nothing exactly matches **%s**. Did you mean:\n", q)
	for _, d := range res.Donors {
		fmt.Fprintf(&sb, "• %s — `%s` points\n", d.Name, short(d.PointsTotal))
	}
	for _, t := range res.Teams {
		fmt.Fprintf(&sb, "• %s (team %d) — `%s` points\n", t.Name, t.TeamID, short(t.PointsTotal))
	}
	return ErrorEmbed(sb.String()), nil
}

func (b *Bot) cmdTop(ctx context.Context, kind, sortKey string, limit int) (*discordgo.MessageEmbed, error) {
	if sortKey == "" {
		sortKey = "lifetime"
	}
	if limit <= 0 || limit > 15 {
		limit = 10
	}
	col := map[string]string{
		"lifetime": "points", "per_day": "per day", "today": "today", "this_week": "this week",
		"this_month": "this month", "last_24h": "last 24h", "wus": "WUs",
	}[sortKey]

	var sb strings.Builder
	var snap Snapshot
	if kind == "teams" {
		ts, s, err := b.api.TopTeams(ctx, sortKey, limit)
		if err != nil {
			return nil, err
		}
		snap = s
		for i, t := range ts {
			fmt.Fprintf(&sb, "%2d. %-28s %10s\n", i+1, trunc(t.Name, 28), short(pick(sortKey, t.PointsTotal, t.PointsPerDay, t.PointsToday, t.PointsLast24h, t.WUsTotal)))
		}
	} else {
		ds, s, err := b.api.TopDonors(ctx, sortKey, limit)
		if err != nil {
			return nil, err
		}
		snap = s
		for i, d := range ds {
			fmt.Fprintf(&sb, "%2d. %-28s %10s\n", i+1, trunc(d.Name, 28), short(pick(sortKey, d.PointsTotal, d.PointsPerDay, d.PointsToday, d.PointsLast24h, d.WUsTotal)))
		}
	}
	title := fmt.Sprintf("Top %d %s by %s", limit, kind, col)
	return TextEmbed(title, SiteURL+"/"+kind+"?sort="+sortKey, sb.String(), snap), nil
}

func pick(key string, lifetime, perDay, today, last24, wus int64) int64 {
	switch key {
	case "per_day":
		return perDay
	case "today", "this_week", "this_month":
		return today
	case "last_24h":
		return last24
	case "wus":
		return wus
	}
	return lifetime
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

/* ---------------------------------------------------------- autocomplete --- */

// handleAutocomplete answers as the user types.
//
// Discord allows three seconds and 25 choices. The search endpoint is a prefix lookup
// over a sorted index, so this costs the service a binary search — which is the reason
// autocomplete is affordable at all, and why the bot never asks anyone to spell
// `[Zebulon.fr]_Gtevoone82` correctly.
func (b *Bot) handleAutocomplete(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.ApplicationCommandData()
	// A subcommand's options are nested one level down, so the focused option is not in
	// data.Options at all — without unwrapping, /alert would offer no completions and
	// look broken in exactly the place a name is hardest to type.
	optList := data.Options
	if len(optList) == 1 && optList[0].Type == discordgo.ApplicationCommandOptionSubCommand {
		optList = optList[0].Options
	}
	var focused *discordgo.ApplicationCommandInteractionDataOption
	for _, o := range optList {
		if o.Focused {
			focused = o
			break
		}
	}
	if focused == nil {
		return
	}
	q := strings.TrimSpace(focused.StringValue())

	// Existing alerts, not names: the only sensible completion for "which one".
	if focused.Name == "alert" {
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionApplicationCommandAutocompleteResult,
			Data: &discordgo.InteractionResponseData{Choices: b.alertChoices(i, q)},
		})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()

	kind := "both"
	for _, o := range optList {
		if o.Name == "kind" {
			kind = o.StringValue()
		}
	}
	if focused.Name == "team" {
		kind = "teams"
	} else if focused.Name == "donor" {
		kind = "donors"
	}

	// One field accepting either kind has to say which it got: a team id and a donor
	// named "51" are the same string otherwise. Only the alert target needs this, so
	// only it gets the prefix.
	tagged := focused.Name == "target"

	var choices []*discordgo.ApplicationCommandOptionChoice
	if q != "" {
		if res, _, err := b.api.Search(ctx, q, 25); err == nil {
			if kind != "donors" {
				for _, t := range res.Teams {
					v := strconv.FormatInt(t.TeamID, 10)
					if tagged {
						v = "t:" + v
					}
					choices = append(choices, &discordgo.ApplicationCommandOptionChoice{
						// The label carries the points so a reader can tell two
						// identically named teams apart; the value is the id, which is
						// what the command actually needs.
						Name:  trunc(fmt.Sprintf("%s (team %d) — %s", t.Name, t.TeamID, short(t.PointsTotal)), 100),
						Value: v,
					})
				}
			}
			if kind != "teams" {
				for _, d := range res.Donors {
					v := trunc(d.Name, 98)
					if tagged {
						v = "d:" + v
					}
					choices = append(choices, &discordgo.ApplicationCommandOptionChoice{
						Name:  trunc(fmt.Sprintf("%s — %s", d.Name, short(d.PointsTotal)), 100),
						Value: v,
					})
				}
			}
		}
	}
	if len(choices) > 25 {
		choices = choices[:25]
	}

	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionApplicationCommandAutocompleteResult,
		Data: &discordgo.InteractionResponseData{Choices: choices},
	})
}

func userID(i *discordgo.InteractionCreate) string {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.ID
	}
	if i.User != nil {
		return i.User.ID
	}
	return ""
}
