// Command archiver polls the upstream Folding@home feeds and stores every new
// snapshot verbatim.
//
// It exists as its own binary so collection can start long before the rest of the
// backend is written: history accrues in real time and cannot be backfilled, so the
// archive is the only part of this project where wall-clock delay is unrecoverable.
// The server will later run the same internal/feed.Archiver in-process.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"folding/internal/feed"
)

func main() {
	var (
		dir      = flag.String("dir", "data", "archive root directory")
		interval = flag.Duration("interval", 10*time.Minute, "poll interval")
		once     = flag.Bool("once", false, "poll once and exit")
		list     = flag.Bool("list", false, "list archived snapshots and exit")
		ua       = flag.String("user-agent", feed.DefaultUserAgent, "HTTP User-Agent")
		verbose  = flag.Bool("v", false, "verbose logging")
	)
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	a, err := feed.New(*dir, *ua, log)
	if err != nil {
		log.Error("startup failed", "err", err)
		os.Exit(1)
	}

	if *list {
		if err := listSnapshots(a); err != nil {
			log.Error("listing failed", "err", err)
			os.Exit(1)
		}
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *once {
		if n := a.Once(ctx); n == 0 {
			log.Info("nothing new")
		}
		return
	}
	a.Run(ctx, *interval)
}

func listSnapshots(a *feed.Archiver) error {
	snaps, err := a.Archive.List("")
	if err != nil {
		return err
	}
	if len(snaps) == 0 {
		fmt.Println("no snapshots archived yet")
		return nil
	}
	var total int64
	for _, s := range snaps {
		fmt.Printf("%s  %-5s  %9s on disk  %s\n",
			s.Meta.SnapshotAt.Format(time.RFC3339),
			s.Meta.Feed,
			humanBytes(s.Meta.StoredBytes),
			s.Meta.FeedTimestamp,
		)
		total += s.Meta.StoredBytes
	}
	fmt.Printf("\n%d snapshots, %s on disk\n", len(snaps), humanBytes(total))
	return nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
