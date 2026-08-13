package bot

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// alertCommand is a subcommand group rather than four top-level commands.
//
// /alert-add, /alert-list and /alert-remove would work identically and read as three
// unrelated things in the picker. Grouped, somebody who types "alert" is shown the
// whole feature, which is the only discovery mechanism Discord offers.
func alertCommand() *discordgo.ApplicationCommand {
	typeChoices := make([]*discordgo.ApplicationCommandOptionChoice, 0, len(AlertKinds))
	for _, k := range AlertKinds {
		typeChoices = append(typeChoices, &discordgo.ApplicationCommandOptionChoice{
			Name: k.Label, Value: string(k.Type),
		})
	}

	return &discordgo.ApplicationCommand{
		Name:        "alert",
		Description: "Get told when a folder or team does something",
		// Alerts write to channels and can ping roles, so configuring them is a
		// moderator action. Discord ignores this in DMs, where the only channel anyone
		// can target is their own.
		DefaultMemberPermissions: ptr(int64(discordgo.PermissionManageServer)),
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type: discordgo.ApplicationCommandOptionSubCommand,
				Name: "add", Description: "Watch a folder or team and post here when something happens",
				Options: []*discordgo.ApplicationCommandOption{
					{
						Type: discordgo.ApplicationCommandOptionString, Name: "type",
						Description: "what to watch for", Required: true, Choices: typeChoices,
					},
					{
						Type: discordgo.ApplicationCommandOptionString, Name: "target",
						Description:  "donor or team — start typing and pick from the list",
						Required:     true,
						Autocomplete: true,
					},
					{
						Type: discordgo.ApplicationCommandOptionChannel, Name: "channel",
						Description: "where to post (default: here)",
						ChannelTypes: []discordgo.ChannelType{
							discordgo.ChannelTypeGuildText,
							discordgo.ChannelTypeGuildNews,
							discordgo.ChannelTypeGuildPublicThread,
							discordgo.ChannelTypeGuildPrivateThread,
						},
					},
					{
						Type: discordgo.ApplicationCommandOptionMentionable, Name: "tag",
						Description: "who to ping when it fires (optional)",
					},
					{
						Type: discordgo.ApplicationCommandOptionInteger, Name: "threshold",
						Description: "rank to reach, hours of silence, or hour of day — depends on type",
						MinValue:    ptr(0.0),
					},
				},
			},
			{
				Type: discordgo.ApplicationCommandOptionSubCommand,
				Name: "list", Description: "Every alert set up here",
			},
			{
				Type: discordgo.ApplicationCommandOptionSubCommand,
				Name: "remove", Description: "Stop an alert",
				Options: []*discordgo.ApplicationCommandOption{{
					Type: discordgo.ApplicationCommandOptionString, Name: "alert",
					Description: "which one", Required: true, Autocomplete: true,
				}},
			},
			{
				Type: discordgo.ApplicationCommandOptionSubCommand,
				Name: "test", Description: "Send a sample of an alert, to check it lands where you meant",
				Options: []*discordgo.ApplicationCommandOption{{
					Type: discordgo.ApplicationCommandOptionString, Name: "alert",
					Description: "which one", Required: true, Autocomplete: true,
				}},
			},
		},
	}
}

// handleAlert owns the whole group, including its own reply.
//
// Separate from the generic dispatch because everything about it differs: it reads
// nested subcommand options, its replies are ephemeral — alert plumbing is
// configuration and does not belong in the channel it configures — and it needs the
// resolved data for the channel and mentionable options.
func (b *Bot) handleAlert(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.ApplicationCommandData()
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral},
	}); err != nil {
		b.log.Error("deferring", "cmd", "alert", "err", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()

	var embed *discordgo.MessageEmbed
	var err error
	if len(data.Options) == 0 {
		embed = ErrorEmbed("Pick a subcommand: add, list, remove or test.")
	} else {
		sub := data.Options[0]
		opts := map[string]*discordgo.ApplicationCommandInteractionDataOption{}
		for _, o := range sub.Options {
			opts[o.Name] = o
		}
		switch sub.Name {
		case "add":
			embed, err = b.alertAdd(ctx, i, &data, opts)
		case "list":
			embed, err = b.alertList(i)
		case "remove":
			embed, err = b.alertRemove(i, optString(opts, "alert"))
		case "test":
			embed, err = b.alertTest(ctx, i, optString(opts, "alert"))
		default:
			embed = ErrorEmbed("Unknown subcommand.")
		}
	}
	if err != nil {
		embed = ErrorEmbed(b.explain(err))
	}
	if _, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Embeds: &[]*discordgo.MessageEmbed{embed},
	}); err != nil {
		b.log.Error("replying", "cmd", "alert", "err", err)
	}
}

// optString and optInt read the raw value rather than calling StringValue/IntValue.
//
// Those helpers panic when the option's declared type does not match, and a panic
// inside an interaction handler takes the goroutine with it — one malformed payload
// would cost the bot rather than cost one reply. Reading the value and type-asserting
// degrades to a zero instead.
func optString(m map[string]*discordgo.ApplicationCommandInteractionDataOption, k string) string {
	if o, ok := m[k]; ok {
		if v, ok := o.Value.(string); ok {
			return v
		}
	}
	return ""
}

func optInt(m map[string]*discordgo.ApplicationCommandInteractionDataOption, k string) (int64, bool) {
	o, ok := m[k]
	if !ok {
		return 0, false
	}
	// Discord sends JSON numbers, which decode as float64.
	switch v := o.Value.(type) {
	case float64:
		return int64(v), true
	case int64:
		return v, true
	case int:
		return int64(v), true
	}
	return 0, false
}

func (b *Bot) alertAdd(ctx context.Context, i *discordgo.InteractionCreate,
	data *discordgo.ApplicationCommandInteractionData,
	opts map[string]*discordgo.ApplicationCommandInteractionDataOption) (*discordgo.MessageEmbed, error) {

	typ := AlertType(optString(opts, "type"))
	k, ok := kindOf(typ)
	if !ok {
		return ErrorEmbed("That is not an alert type."), nil
	}

	// Threshold before the lookup: a bad number should not cost a request, and the
	// message explaining it is more useful than one about a donor.
	// Whatever was typed, verbatim — 0 is a real answer for the daily hour, and for the
	// other types it is a mistake that deserves to be explained rather than replaced by
	// a default the user never asked for and would not see.
	threshold := k.Default
	if v, ok := optInt(opts, "threshold"); ok {
		threshold = v
	}
	if msg := checkThreshold(typ, threshold); msg != "" {
		return ErrorEmbed(msg), nil
	}

	kind, target, err := b.resolveTarget(ctx, optString(opts, "target"))
	if err != nil {
		return nil, err
	}
	if kind == "" {
		return b.suggest(ctx, optString(opts, "target"))
	}
	e, err := b.reading(ctx, kind, target)
	if err != nil {
		return nil, err
	}

	channelID := i.ChannelID
	// Same reasoning as optString: ChannelValue panics on a type mismatch, and the id
	// is all that is wanted anyway.
	if o, ok := opts["channel"]; ok {
		if id, ok := o.Value.(string); ok && id != "" {
			channelID = id
		}
	}

	a := &Alert{
		GuildID:   i.GuildID,
		ChannelID: channelID,
		Type:      typ,
		Kind:      kind,
		Target:    target,
		Label:     e.Name,
		Threshold: threshold,
		Tag:       mentionFrom(data, opts["tag"]),
		CreatedBy: userID(i),
		CreatedAt: time.Now().UTC(),
	}
	seed(a, e, time.Now().UTC())

	// Prove the channel works before storing anything, by posting there.
	//
	// The alternative is inspecting permissions, which needs guild state this bot
	// deliberately does not hold, and which can still be wrong about threads and
	// channel overrides. Sending is the only check that tests the thing that matters,
	// and it doubles as telling the channel what is about to start appearing in it.
	if err := b.announce(a); err != nil {
		return ErrorEmbed(fmt.Sprintf(
			"I cannot post in <#%s>, so that alert would never arrive. Give me **View Channel** "+
				"and **Send Messages** there and run this again.\n\nDiscord said: %s",
			channelID, err)), nil
	}
	if err := b.alerts.Add(a); err != nil {
		return ErrorEmbed(err.Error()), nil
	}

	return &discordgo.MessageEmbed{
		Title: "Alert added", Color: colourGood, URL: a.TargetURL(),
		Description: fmt.Sprintf("**%s**\nPosting in <#%s>%s.\n\nRemove it with **/alert remove**.",
			mdEsc(a.Describe()), channelID, tagSuffix(a.Tag)),
	}, nil
}

func checkThreshold(t AlertType, v int64) string {
	switch t {
	case AlertRank:
		if v < 1 {
			return "A rank alert needs a **threshold** of 1 or more — the rank to reach."
		}
	case AlertIdle:
		if v < 1 || v > 720 {
			return "Hours of silence must be between **1** and **720**. Below an hour would fire " +
				"on the gap between two upstream publishes."
		}
	case AlertDaily:
		if v < 0 || v > 23 {
			return "The hour must be between **0** and **23**, in UTC."
		}
	}
	return ""
}

// mentionFrom renders the tag option as the mention text that will ping it.
//
// A mentionable option carries only a snowflake, and a user and a role with the same
// id render differently — "<@id>" against "<@&id>". Getting it wrong produces a
// message with a dead-looking mention that pings nobody, which is the failure this
// feature exists to avoid.
func mentionFrom(data *discordgo.ApplicationCommandInteractionData, o *discordgo.ApplicationCommandInteractionDataOption) string {
	if o == nil || data.Resolved == nil {
		return ""
	}
	id, _ := o.Value.(string)
	if id == "" {
		return ""
	}
	if _, ok := data.Resolved.Roles[id]; ok {
		return "<@&" + id + ">"
	}
	if _, ok := data.Resolved.Users[id]; ok {
		return "<@" + id + ">"
	}
	return ""
}

func tagSuffix(tag string) string {
	if tag == "" {
		return ""
	}
	return ", pinging " + tag
}

// resolveTarget turns the target option into a kind and a key.
func (b *Bot) resolveTarget(ctx context.Context, raw string) (kind, target string, err error) {
	kind, target, tagged := parseTarget(raw)
	if tagged {
		return kind, target, nil
	}
	if target == "" {
		return "", "", nil
	}
	// Typed rather than picked. A bare number is a team id; otherwise try an exact
	// donor, then fall back to the best search hit.
	if id, convErr := strconv.ParseInt(target, 10, 64); convErr == nil {
		if _, _, e := b.api.Team(ctx, id); e == nil {
			return "team", target, nil
		} else if !NotFound(e) {
			return "", "", e
		}
	}
	if _, _, e := b.api.Donor(ctx, target); e == nil {
		return "donor", target, nil
	} else if !NotFound(e) {
		return "", "", e
	}
	res, _, e := b.api.Search(ctx, target, 5)
	if e != nil {
		return "", "", e
	}
	if len(res.Donors) > 0 {
		return "donor", res.Donors[0].Name, nil
	}
	if len(res.Teams) > 0 {
		return "team", strconv.FormatInt(res.Teams[0].TeamID, 10), nil
	}
	return "", "", nil
}

func (b *Bot) alertList(i *discordgo.InteractionCreate) (*discordgo.MessageEmbed, error) {
	list := b.alerts.InScope(i.GuildID, i.ChannelID)
	if len(list) == 0 {
		return &discordgo.MessageEmbed{
			Title: "No alerts yet", Color: colourNormal,
			Description: "Set one up with **/alert add** — pick a folder or team, and what you " +
				"want to hear about.",
		}, nil
	}
	var sb strings.Builder
	for _, a := range list {
		fmt.Fprintf(&sb, "`%s` %s\n<#%s>%s\n\n", a.ID, mdEsc(a.Describe()), a.ChannelID, tagSuffix(a.Tag))
	}
	return &discordgo.MessageEmbed{
		Title:       fmt.Sprintf("%s here", plural(len(list), "alert")),
		Color:       colourNormal,
		Description: strings.TrimSpace(sb.String()),
		Footer: &discordgo.MessageEmbedFooter{
			Text: "Remove one with /alert remove, or check delivery with /alert test",
		},
	}, nil
}

func (b *Bot) alertRemove(i *discordgo.InteractionCreate, id string) (*discordgo.MessageEmbed, error) {
	a, ok := b.alerts.Get(id)
	if !ok || !inScope(a, i) {
		return ErrorEmbed("No alert with that id here. Run **/alert list** to see them."), nil
	}
	if _, err := b.alerts.Remove(id); err != nil {
		return nil, err
	}
	return &discordgo.MessageEmbed{
		Title: "Alert removed", Color: colourNormal, Description: mdEsc(a.Describe()),
	}, nil
}

// alertTest proves delivery, which is the question people actually have.
//
// "Did I set this up right" cannot be answered by waiting — a milestone alert may not
// fire for months, and silence is indistinguishable from a permissions mistake.
func (b *Bot) alertTest(ctx context.Context, i *discordgo.InteractionCreate, id string) (*discordgo.MessageEmbed, error) {
	a, ok := b.alerts.Get(id)
	if !ok || !inScope(a, i) {
		return ErrorEmbed("No alert with that id here. Run **/alert list** to see them."), nil
	}
	var snap Snapshot
	if env, err := b.api.GetEnvelope(ctx, "/v1/status"); err == nil {
		snap = env.Snapshot
	}
	e := AlertEmbed(a, "Test — "+a.Label, "This is what **"+mdEsc(a.Describe())+
		"** will look like. Nothing has actually happened.", snap)
	// deliver mutates the alert's failure count, and the watcher does the same on its
	// own schedule; without this the two race on the same struct.
	b.alertMu.Lock()
	err := b.deliver(a, e)
	b.alertMu.Unlock()
	if err != nil {
		return ErrorEmbed(fmt.Sprintf("Could not post in <#%s>: %s", a.ChannelID, err)), nil
	}
	return &discordgo.MessageEmbed{
		Title: "Sent", Color: colourGood,
		Description: fmt.Sprintf("A sample went to <#%s>.", a.ChannelID),
	}, nil
}

// inScope keeps one server from touching another's alerts.
func inScope(a *Alert, i *discordgo.InteractionCreate) bool {
	if i.GuildID != "" {
		return a.GuildID == i.GuildID
	}
	return a.ChannelID == i.ChannelID
}

// announce posts the confirmation that doubles as the permission check.
func (b *Bot) announce(a *Alert) error {
	if b.session == nil {
		return nil // tests drive the handlers without a session
	}
	_, err := b.session.ChannelMessageSendComplex(a.ChannelID, &discordgo.MessageSend{
		Embeds: []*discordgo.MessageEmbed{{
			Title: "Alerts on", Color: colourNormal, URL: a.TargetURL(),
			Description: fmt.Sprintf("**%s**\nThis channel will get a message when it happens.", mdEsc(a.Describe())),
		}},
	})
	return err
}

// alertChoices answers autocomplete for the remove and test subcommands.
func (b *Bot) alertChoices(i *discordgo.InteractionCreate, q string) []*discordgo.ApplicationCommandOptionChoice {
	var out []*discordgo.ApplicationCommandOptionChoice
	q = strings.ToLower(q)
	for _, a := range b.alerts.InScope(i.GuildID, i.ChannelID) {
		d := a.Describe()
		if q != "" && !strings.Contains(strings.ToLower(d), q) {
			continue
		}
		out = append(out, &discordgo.ApplicationCommandOptionChoice{
			Name: trunc(d, 100), Value: a.ID,
		})
		if len(out) == 25 {
			break
		}
	}
	return out
}
