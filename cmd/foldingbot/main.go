// Command foldingbot answers Folding@home questions in Discord.
//
// It runs beside the statistics service rather than on it, and reaches it over the
// internal bridge: a bot resolving the public hostname would leave the network,
// traverse the CDN and come back through the uplink to reach a machine one bridge
// away — spending the household's bandwidth on a round trip that never needed to
// leave the box.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"folding/internal/bot"
)

func main() {
	var (
		api     = flag.String("api", envOr("FOLDING_API", "http://10.10.10.55:8080"), "statistics API base URL — the private address")
		site    = flag.String("site", envOr("FOLDING_SITE_URL", "https://foldingstats.org"), "public site URL, used for links inside replies")
		links   = flag.String("links", envOr("FOLDING_LINKS", "/var/lib/foldingbot/links.json"), "where donor links are stored")
		alerts  = flag.String("alerts", envOr("FOLDING_ALERTS", "/var/lib/foldingbot/alerts.json"), "where channel alerts are stored")
		guild   = flag.String("guild", os.Getenv("DISCORD_GUILD_ID"), "register commands to one guild (instant) instead of globally (up to an hour)")
		verbose = flag.Bool("v", false, "verbose logging")
	)
	flag.Parse()

	lvl := slog.LevelInfo
	if *verbose {
		lvl = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))

	// The token is read from the environment and never from a flag: flags are visible
	// in /proc to anything on the box and land in shell history.
	token := os.Getenv("DISCORD_TOKEN")
	if token == "" {
		log.Error("DISCORD_TOKEN is not set")
		os.Exit(1)
	}

	b, err := bot.New(bot.Config{
		Token:      token,
		APIBase:    *api,
		SiteURL:    *site,
		LinksPath:  *links,
		AlertsPath: *alerts,
		GuildID:    *guild,
		Log:        log,
	})
	if err != nil {
		log.Error("starting", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := b.Run(ctx); err != nil {
		log.Error("running", "err", err)
		os.Exit(1)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
