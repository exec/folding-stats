package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/bwmarrin/discordgo"
)

type Bot struct {
	api    *Client
	links  *Links
	alerts *Alerts
	log    *slog.Logger

	// alertMu guards mutation of stored alerts. The watcher writes their state on every
	// snapshot and /alert test writes their failure count on demand; the store's own
	// lock covers the map, not the structs it holds.
	alertMu sync.Mutex

	session *discordgo.Session
	guildID string // when set, commands register instantly to one guild
	mcpURL  string
}

type Config struct {
	Token      string
	APIBase    string // the private address, e.g. http://10.10.10.55:8080
	SiteURL    string // the public name, for links inside embeds
	LinksPath  string
	AlertsPath string
	GuildID    string
	Log        *slog.Logger
}

func New(cfg Config) (*Bot, error) {
	if cfg.Token == "" {
		return nil, fmt.Errorf("bot: no Discord token")
	}
	if cfg.SiteURL != "" {
		SiteURL = cfg.SiteURL
	}
	links, err := OpenLinks(cfg.LinksPath)
	if err != nil {
		return nil, err
	}
	alerts, err := OpenAlerts(cfg.AlertsPath)
	if err != nil {
		return nil, err
	}
	s, err := discordgo.New("Bot " + cfg.Token)
	if err != nil {
		return nil, err
	}
	// Slash commands arrive as interactions over the gateway, so the bot needs no
	// privileged intents and reads no message content. It cannot see what anyone
	// says, which is both the least it needs and the least it should have.
	s.Identify.Intents = discordgo.IntentsNone

	b := &Bot{
		api:     NewClient(cfg.APIBase),
		links:   links,
		alerts:  alerts,
		log:     cfg.Log,
		session: s,
		guildID: cfg.GuildID,
		mcpURL:  cfg.APIBase + "/mcp",
	}
	s.AddHandler(func(_ *discordgo.Session, r *discordgo.Ready) {
		b.log.Info("connected", "user", r.User.Username+"#"+r.User.Discriminator, "guilds", len(r.Guilds))
	})
	s.AddHandler(func(sess *discordgo.Session, i *discordgo.InteractionCreate) {
		switch i.Type {
		case discordgo.InteractionApplicationCommand:
			b.handleCommand(sess, i)
		case discordgo.InteractionApplicationCommandAutocomplete:
			b.handleAutocomplete(sess, i)
		}
	})
	return b, nil
}

// Run connects and blocks until ctx is cancelled.
func (b *Bot) Run(ctx context.Context) error {
	if err := b.session.Open(); err != nil {
		return fmt.Errorf("connecting to Discord: %w", err)
	}
	defer b.session.Close()

	if err := b.register(); err != nil {
		return err
	}
	b.log.Info("ready", "commands", len(Commands()), "links", b.links.Count(),
		"alerts", b.alerts.Count(), "api", b.api.Base)

	// The watcher is the only thing here that outlives an interaction, so it is tied to
	// the same context: a shutdown stops it before the session closes underneath it.
	go b.watch(ctx)

	<-ctx.Done()
	b.log.Info("shutting down")
	return nil
}

// register publishes the command set.
//
// Registered per-guild when a guild is configured and globally otherwise: global
// commands can take up to an hour to appear, which makes iterating on them
// unbearable, while guild commands are visible immediately.
func (b *Bot) register() error {
	appID := b.session.State.User.ID
	cmds := Commands()
	_, err := b.session.ApplicationCommandBulkOverwrite(appID, b.guildID, cmds)
	if err != nil {
		return fmt.Errorf("registering commands: %w", err)
	}
	scope := "globally"
	if b.guildID != "" {
		scope = "to guild " + b.guildID
	}
	b.log.Info("commands registered", "count", len(cmds), "scope", scope)
	return nil
}

/* -------------------------------------------------------------------- mcp --- */

// mcpEmbed answers through the service's MCP endpoint rather than its REST routes.
//
// Those tools already compose the multi-step questions — rivals, head-to-head, what a
// goal would cost — and, more importantly, they return prose that states its own
// assumptions: that a projection holds both sides at today's average, that an entity
// which did not exist yesterday has no rank change. Rebuilding those answers here
// would mean rebuilding the caveats too, and a caveat reimplemented in a second place
// is a caveat that will eventually disagree with the first.
func (b *Bot) mcpEmbed(ctx context.Context, tool string, args map[string]any, title, url string) (*discordgo.MessageEmbed, error) {
	text, err := b.callMCP(ctx, tool, args)
	if err != nil {
		return nil, err
	}
	// The tools carry their own "data as of" line, so the embed footer would say it
	// twice. Strip theirs and keep the structured one.
	var snap Snapshot
	if env, err := b.api.GetEnvelope(ctx, "/v1/status"); err == nil {
		snap = env.Snapshot
	}
	if i := strings.Index(text, "\nData as of "); i > 0 {
		text = strings.TrimRight(text[:i], "\n")
	}
	if url == "" {
		url = SiteURL
	}
	return TextEmbed(title, url, text, snap), nil
}

type mcpResult struct {
	Result struct {
		IsError bool `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"result"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (b *Bot) callMCP(ctx context.Context, tool string, args map[string]any) (string, error) {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args},
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.mcpURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := b.api.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("reaching the statistics service: %w", err)
	}
	defer resp.Body.Close()

	var out mcpResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decoding the tool result: %w", err)
	}
	if out.Error != nil {
		return "", &APIError{Status: resp.StatusCode, Message: out.Error.Message}
	}
	var sb string
	for _, c := range out.Result.Content {
		sb += c.Text
	}
	if out.Result.IsError {
		// A tool error is the service explaining a bad argument, which is exactly
		// what the user needs to read.
		return "", &APIError{Status: http.StatusBadRequest, Message: sb}
	}
	if sb == "" {
		return "", fmt.Errorf("the tool returned nothing")
	}
	return sb, nil
}
