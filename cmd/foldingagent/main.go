// Command foldingagent puts one machine's folding client on your dashboard.
//
// Install it on any machine that folds — your desktop, a box in a cupboard, a rented
// GPU — and it appears at https://folding.exec.codes/fold beside the rest. It reads
// the folding client on 127.0.0.1 and holds one outbound connection to the relay, so
// it needs no inbound port, no port forwarding, no certificate and no change to the
// folding client's own configuration.
//
// It never sends a credential anywhere. The machine's private key is generated here on
// first run and stays on disk; an enrolment token, valid once and for half an hour,
// is the only thing that travels, and only until the relay has seen this machine.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"folding/internal/agent"
	p "folding/internal/relayproto"
)

func main() {
	host, _ := os.Hostname()
	var (
		relay = flag.String("relay", envOr("FOLDING_RELAY", "wss://folding.exec.codes/relay/agent"),
			"relay endpoint")
		local = flag.String("local", envOr("FOLDING_CLIENT", "ws://127.0.0.1:7396/api/websocket"),
			"the folding client on this machine")
		keyPath = flag.String("key", envOr("FOLDING_KEY", "/var/lib/foldingagent/machine.key"),
			"where this machine's identity is kept")
		name    = flag.String("name", envOr("FOLDING_NAME", host), "how this machine appears in your fleet")
		showKey = flag.Bool("key-only", false, "print this machine's public key and exit")
		verbose = flag.Bool("v", false, "verbose logging")
	)
	flag.Parse()

	lvl := slog.LevelInfo
	if *verbose {
		lvl = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))

	cfg := agent.Config{
		Relay: *relay, Local: *local, KeyPath: *keyPath, Name: *name, Log: log,
	}

	// The token is read from the environment rather than a flag: flags are visible in
	// /proc to anything on the box and land in shell history, and this one authorises
	// adding a machine to somebody's fleet.
	if raw := os.Getenv("FOLDING_ENROL"); raw != "" {
		var tok p.Enrolment
		if err := json.Unmarshal([]byte(raw), &tok); err != nil {
			log.Error("FOLDING_ENROL is not a valid enrolment token", "err", err)
			os.Exit(1)
		}
		cfg.Token = &tok
	}

	a, err := agent.New(cfg)
	if err != nil {
		log.Error("starting", "err", err)
		os.Exit(1)
	}
	if *showKey {
		fmt.Println(a.Key())
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := a.Run(ctx); err != nil {
		log.Error("agent stopped", "err", err)
		os.Exit(1)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
