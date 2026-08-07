// Command foldingrelay forwards frames between a browser and the folding agents it
// owns.
//
// It runs beside the statistics service rather than inside it. The site serves tens of
// thousands of stateless reads a second from an in-memory snapshot; a relay holds
// thousands of idle sockets open for hours. Those fail in different ways and at
// different times, and a bug in the newer of the two should not be able to take the
// older one down with it.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"folding/internal/relay"
)

func main() {
	var (
		addr    = flag.String("addr", envOr("RELAY_ADDR", "127.0.0.1:8090"), "listen address")
		store   = flag.String("store", envOr("RELAY_STORE", "/var/lib/foldingrelay/machines.json"), "where enrolments are recorded")
		verbose = flag.Bool("v", false, "verbose logging")
	)
	flag.Parse()

	lvl := slog.LevelInfo
	if *verbose {
		lvl = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))

	st, err := relay.OpenStore(*store)
	if err != nil {
		log.Error("opening the enrolment store", "err", err)
		os.Exit(1)
	}
	hub := relay.New(st, log)

	srv := &http.Server{
		Addr:    *addr,
		Handler: hub.Handler(),
		// No read or write timeout: these connections are meant to sit idle for hours.
		// The protocol's own ping keeps them honest, and an unauthenticated one is shut
		// by the handshake deadline long before it can cost anything.
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Info("relay listening", "addr", *addr, "machines", st.Count())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("serving", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	sh, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(sh)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
