// Command foldingd runs the stats service: it archives the upstream feeds, ingests
// whatever the archive holds, and serves the public API.
//
// Archiving and ingest are separate stages on purpose. The archiver only has to get
// bytes onto disk; ingest reads them back. So a crash mid-ingest costs a restart
// rather than a hole in the history, and changing how a metric is derived is a
// replay rather than a loss.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"folding/internal/api"
	"folding/internal/feed"
	"folding/internal/service"
	"folding/internal/store"
	"folding/web"
)

func main() {
	var (
		dir       = flag.String("dir", "data", "data directory (archive + database)")
		addr      = flag.String("addr", ":8080", "HTTP listen address")
		poll      = flag.Duration("poll", 10*time.Minute, "upstream poll interval")
		ua        = flag.String("user-agent", feed.DefaultUserAgent, "HTTP User-Agent for upstream")
		noFetch   = flag.Bool("no-fetch", false, "serve from the existing archive without polling upstream")
		compact   = flag.Duration("compact-after", 90*24*time.Hour, "age at which raw deltas are rolled up")
		keepDaily = flag.Duration("keep-daily", 2*365*24*time.Hour, "age at which daily rollups collapse to monthly (0 keeps them)")
		keepRaw   = flag.Duration("keep-raw", 90*24*time.Hour, "age at which archived snapshots thin to one per UTC day (0 keeps every snapshot)")
		verbose   = flag.Bool("v", false, "verbose logging")
	)
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if err := run(*dir, *addr, *ua, *poll, *compact, *keepDaily, *keepRaw, *noFetch, log); err != nil &&
		!errors.Is(err, context.Canceled) && !errors.Is(err, http.ErrServerClosed) {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(dir, addr, ua string, poll, compactAfter, keepDaily, keepRaw time.Duration, noFetch bool, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	archiver, err := feed.New(dir, ua, log)
	if err != nil {
		return err
	}
	db, err := store.Open(dir + "/history.db")
	if err != nil {
		return err
	}
	defer db.Close()

	srv := api.NewServer()
	svc, err := service.New(archiver.Archive, db, srv, log)
	if err != nil {
		return err
	}

	// The API owns /v1; everything else is the frontend, including client-side
	// routes that must deep-link.
	site, err := web.Handler()
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/", srv)
	mux.Handle("/", site)

	// Catch up on anything archived while we were down before accepting traffic,
	// so the first request served is never against a half-built view.
	if n, err := svc.Ingest(ctx); err != nil {
		log.Error("initial ingest failed", "err", err)
	} else {
		log.Info("initial ingest complete", "cycles", n)
	}

	// A newly archived snapshot triggers ingest immediately rather than waiting for
	// the next tick, so the API is current within seconds of upstream publishing.
	archiver.OnSnapshot = func(feed.Snapshot) {
		if n, err := svc.Ingest(ctx); err != nil {
			log.Error("ingest failed", "err", err)
		} else if n > 0 {
			log.Info("ingested", "cycles", n)
		}
	}

	// Poll on the schedule the service predicts rather than a flat tick. A fixed
	// interval spreads the gap between upstream publishing and us capturing it evenly
	// across that interval — five minutes on average at the default. That lag is
	// invisible in the data but very visible to a client counting down to the next
	// update, which would reach zero and then wait out the rest of a tick.
	archiver.Delay = func(now time.Time) time.Duration { return svc.PollDelay(now, poll) }

	// Background work is waited on, not abandoned. `defer db.Close()` above used to
	// fire as soon as ListenAndServe returned, while the archiver goroutine could
	// still be inside a cycle's write and maintain inside a compaction — closing the
	// database out from under either. An aborted cycle write is not a clean rollback
	// in effect: the in-memory model has already absorbed the snapshot destructively,
	// so the hour is unrecoverable and the slots it assigned are never persisted.
	var bg sync.WaitGroup
	bg.Add(2)
	go func() {
		defer bg.Done()
		if noFetch {
			log.Warn("upstream polling disabled; following the existing archive")
			follow(ctx, svc, poll, log)
			return
		}
		archiver.Run(ctx, poll)
	}()

	go func() {
		defer bg.Done()
		maintain(ctx, svc, db, archiver.Archive, compactAfter, keepDaily, keepRaw, log)
	}()

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// Generous: a history query over a long window can be slow, and the whole
		// point is that clients need not poll aggressively.
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdown)
	}()

	log.Info("serving", "addr", addr, "dir", dir)
	err = httpSrv.ListenAndServe()

	// However the server ended — a signal or a failure to listen — stop the
	// background work and let it finish before the deferred db.Close() runs. Without
	// the explicit stop, a listen failure would leave ctx live and this would block
	// forever instead of reporting the error.
	stop()
	bg.Wait()
	log.Info("background work finished")
	return err
}

// follow serves an archive somebody else is filling.
//
// -no-fetch used to mean "ingest once at startup and then nothing", which is not
// what "serve the existing archive" implies and is not useful for anything: a
// follower froze at whatever the archive held when it booted, and its countdown ran
// to zero and stayed there.
//
// Instead it now scans for new snapshots on the same predicted schedule the fetcher
// would have used, minus the fetching. That makes a second instance — a review copy,
// a read replica, a machine sharing an NFS mount — cost upstream exactly nothing,
// which is the whole reason to run one this way rather than pointing another
// collector at their origin.
func follow(ctx context.Context, svc *service.Service, poll time.Duration, log *slog.Logger) {
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := svc.Ingest(ctx); err != nil {
				log.Error("ingest failed", "err", err)
			} else if n > 0 {
				log.Info("ingested from archive", "cycles", n)
			}
			t.Reset(svc.PollDelay(time.Now().UTC(), poll))
		}
	}
}

// maintain republishes periodically so the staleness flag tracks real time, and
// compacts history once a day.
func maintain(ctx context.Context, svc *service.Service, db *store.Store, archive *feed.Archive,
	compactAfter, keepDaily, keepRaw time.Duration, log *slog.Logger) {

	refresh := time.NewTicker(5 * time.Minute)
	defer refresh.Stop()
	compact := time.NewTicker(24 * time.Hour)
	defer compact.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-refresh.C:
			svc.Refresh()
		case <-compact.C:
			start := time.Now()
			now := time.Now().UTC()
			policy := store.CompactPolicy{RawBefore: now.Add(-compactAfter)}
			if keepDaily > 0 {
				policy.DailyBefore = now.Add(-keepDaily)
			}
			res, err := db.Compact(ctx, policy)
			if err != nil {
				log.Error("compaction failed", "err", err)
				continue
			}
			log.Info("compacted",
				"pruned_raw", res.PrunedRaw, "pruned_daily", res.PrunedDaily,
				"took", time.Since(start).Round(time.Millisecond))

			// Thin the raw archive on the same daily pass. This was written, tested
			// and documented as implemented, and never called from anywhere — the
			// archive grew unthinned at ~282 GB/year against a 600 GB volume, which
			// fills in a little over two years. Filling it is not a degraded mode: a
			// snapshot that cannot be written is gone, because upstream overwrites
			// each file and nothing can backfill it.
			if keepRaw > 0 {
				start = time.Now()
				pr, err := archive.Prune(feed.RetentionPolicy{
					FullResolution: keepRaw, KeepDailyAfter: true,
				}, time.Now().UTC())
				if err != nil {
					log.Error("archive prune failed", "err", err)
					continue
				}
				if err := archive.PruneEmptyDirs(); err != nil {
					log.Warn("archive prune: removing empty directories", "err", err)
				}
				log.Info("archive pruned",
					"kept", pr.Kept, "deleted", pr.Deleted,
					"freed_mb", pr.Freed/(1<<20),
					"took", time.Since(start).Round(time.Millisecond))
			}
		}
	}
}
