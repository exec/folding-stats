// Command gendata writes a synthetic archive for benchmarking and load testing.
//
// The archive only accumulates in real time, so exercising a year of history — or
// compaction, or the rank tables at full corpus size — would otherwise mean waiting a
// year. Generated snapshots use the real feed format and go into a real archive, so
// everything downstream runs the production code path unmodified.
//
// Fidelity matters more than realism-for-its-own-sake. The distributions in corpus.go
// are taken from the measured 2026-08-02 corpus, because the properties that stress
// this system — a handful of names on thousands of teams, 99.96% of donors idle in any
// hour, one team holding a third of all members — are exactly the ones a naive uniform
// generator would smooth away.
//
// Nothing here is served or shipped: the generator lives in this command because this
// command is its only consumer, and no part of foldingd links against it.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"folding/internal/feed"
)

func main() {
	var (
		dir      = flag.String("dir", "data", "archive directory to write into")
		cache    = flag.String("cache", "", "wordlist cache directory (default: <dir>)")
		members  = flag.Int("members", 200_000, "number of (name, team) pairs")
		teams    = flag.Int("teams", 0, "number of teams (0 scales with members)")
		cycles   = flag.Int("cycles", 168, "number of publish cycles to generate")
		interval = flag.Duration("interval", time.Hour, "spacing between cycles")
		endStr   = flag.String("end", "", "timestamp of the final cycle (RFC3339, default now)")
		seed     = flag.Int64("seed", 1, "random seed")
		inspect  = flag.String("inspect", "", "print the head of the newest snapshot (\"user\" or \"team\") and exit")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if *inspect != "" {
		if err := inspectArchive(filepath.Join(*dir, "raw"), *inspect); err != nil {
			log.Error("inspect", "err", err)
			os.Exit(1)
		}
		return
	}

	end := time.Now().UTC().Truncate(time.Hour)
	if *endStr != "" {
		t, err := time.Parse(time.RFC3339, *endStr)
		if err != nil {
			log.Error("invalid -end", "err", err)
			os.Exit(1)
		}
		end = t.UTC()
	}

	cacheDir := *cache
	if cacheDir == "" {
		cacheDir = *dir
	}
	log.Info("loading wordlist", "cache", cacheDir)
	words, err := Words(cacheDir)
	if err != nil {
		log.Error("wordlist", "err", err)
		os.Exit(1)
	}

	cfg := DefaultConfig().Scale(*members)
	cfg.Seed = *seed
	if *teams > 0 {
		cfg.Teams = *teams
	}

	log.Info("building corpus", "members", cfg.Members, "teams", cfg.Teams, "words", len(words))
	start := time.Now()
	corpus, err := New(cfg, words)
	if err != nil {
		log.Error("corpus", "err", err)
		os.Exit(1)
	}
	log.Info("corpus built", "took", time.Since(start).Round(time.Millisecond))

	archive := &feed.Archive{Root: filepath.Join(*dir, "raw")}
	log.Info("generating", "cycles", *cycles, "interval", *interval,
		"span", (time.Duration(*cycles-1) * *interval).Round(time.Hour), "end", end.Format(time.RFC3339))

	start = time.Now()
	first, err := corpus.Generate(archive, end, *cycles, *interval, func(i int, at time.Time) {
		if i%50 == 0 || i == *cycles-1 {
			log.Info("cycle", "n", i+1, "of", *cycles, "at", at.Format(time.RFC3339))
		}
	})
	if err != nil {
		log.Error("generate", "err", err)
		os.Exit(1)
	}

	var bytes int64
	for _, k := range feed.All() {
		snaps, _ := archive.List(k)
		for _, s := range snaps {
			bytes += s.Meta.StoredBytes
		}
	}
	fmt.Printf("\ngenerated %d cycles spanning %s → %s in %s\narchive: %.1f MB on disk\n",
		*cycles, first.Format(time.RFC3339), end.Format(time.RFC3339),
		time.Since(start).Round(time.Second), float64(bytes)/(1<<20))
}

// inspectArchive prints the head of the newest snapshot, so generated output can be
// eyeballed against the real feed format without decompressing by hand.
func inspectArchive(root, which string) error {
	kind := feed.Users
	if which == "team" {
		kind = feed.Teams
	}
	a := &feed.Archive{Root: root}
	s, ok, err := a.Latest(kind)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no %s snapshot in %s", kind, root)
	}
	rc, err := s.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	fmt.Printf("%s  %s  (%.1f MB stored, %.1f MB raw)\n", kind,
		s.Meta.SnapshotAt.Format(time.RFC3339),
		float64(s.Meta.StoredBytes)/(1<<20), float64(s.Meta.Bytes)/(1<<20))

	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for i := 0; i < 14 && sc.Scan(); i++ {
		fmt.Printf("  %q\n", sc.Text())
	}
	return sc.Err()
}
