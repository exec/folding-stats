package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The driver runs against the real embedded charts.js, not a transcription of its
// arithmetic. A copy of the formula in Go would pass while the shipped one was wrong,
// which is the only failure this test exists to catch.
const perDayDriver = `
import { perDayPoints } from './charts.mjs';

const until = Date.parse('2026-08-04T12:00:00Z');
const rate = (at, points, granularity, since) =>
  perDayPoints([{ at, points, wus: 0 }], granularity, until, since)[0].points;

// Two days of history, as the live service had the day this was written.
const twoDays = until - 2 * 86400e3;

const cases = [
  // A bucket that closed before the snapshot is divided by its whole length.
  ['finished day',   rate('2026-08-01T00:00:00Z', 1000, 'daily'),    1000],
  ['finished week',  rate('2026-07-19T00:00:00Z', 7000, 'weekly'),   1000],
  ['finished month', rate('2026-07-01T00:00:00Z', 3100, 'monthly'),  100],
  // July has 31 days and June 30, so a fixed 30-day divisor would be wrong for both.
  ['june length',    rate('2026-06-01T00:00:00Z', 3000, 'monthly'),  100],
  // Hourly deltas are complete on arrival and scale up, never down.
  ['hourly',         rate('2026-08-04T11:00:00Z', 100,  'hourly'),   2400],
  // The bucket still in progress is divided by the part of it that has elapsed.
  // Halfway through the day, 1000 points is a 2000/day pace — dividing by a whole
  // day instead would report the newest bar at half the rate it is really running.
  ['day in progress', rate('2026-08-04T00:00:00Z', 1000, 'daily'),   2000],
  // 2.5 days into the week beginning Sunday 2 August: the divisor is the 2.5 days
  // that have happened, not the 7 the bucket nominally spans.
  ['week in progress', rate('2026-08-02T00:00:00Z', 1000, 'weekly'), 400],
  // The floor. One minute past midnight UTC the elapsed span is near zero, and an
  // unclamped divisor sends the newest bar off the top of the chart.
  ['floor', rate('2026-08-04T11:59:00Z', 1000, 'daily'), 24000],

  // A young service. The week began on Sunday the 2nd but nothing was recorded
  // before the 2nd at noon, so the bucket holds two days of production, not three,
  // and 2000/day is the rate that agrees with what the daily and hourly views show.
  // Dividing by the three days the bucket spans would report 1333 and put the coarse
  // views permanently below the fine ones.
  ['unobserved week',  rate('2026-08-02T00:00:00Z', 4000, 'weekly',  twoDays), 2000],
  ['unobserved month', rate('2026-08-01T00:00:00Z', 4000, 'monthly', twoDays), 2000],
  // Once the bucket is inside the observed span the history bound stops mattering.
  ['observed day', rate('2026-08-03T00:00:00Z', 1000, 'daily', twoDays), 1000],
];

let bad = 0;
for (const [name, got, want] of cases) {
  if (got !== want) {
    console.log('FAIL ' + name + ': got ' + got + ', want ' + want);
    bad++;
  }
}
if (!bad) console.log('OK');
`

// TestPerDayPoints pins the rate arithmetic behind the chart's "Per day" toggle.
//
// The correction that matters is the in-progress bucket. Today, this week and this
// month are all partial whenever anyone is looking, and dividing them by their
// nominal length reports a collapse that has not happened — the same mistake that
// once had the project's headline per-day figure reading 8.8x low. There is no way
// to eyeball it either: the bar looks perfectly plausible at any wrong scale.
func TestPerDayPoints(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping per-day arithmetic check")
	}

	dir := t.TempDir()
	names, err := scriptNames()
	if err != nil {
		t.Fatal(err)
	}
	// charts.js reaches format.js and uPlot by absolute path, which is how the browser
	// resolves them and how node cannot. Copy the whole graph and rewrite the
	// specifiers to the .mjs siblings beside it.
	abs := regexp.MustCompile(`'/(vendor/)?([A-Za-z.]+)\.(esm\.js|js)'`)
	for _, name := range names {
		src, err := assets.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		out := abs.ReplaceAllString(string(src), "'./${1}${2}.mjs'")
		dst := filepath.Join(dir, strings.TrimSuffix(name, ".js")+".mjs")
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, []byte(out), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "driver.mjs"), []byte(perDayDriver), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := exec.Command(node, filepath.Join(dir, "driver.mjs")).CombinedOutput()
	if err != nil {
		t.Fatalf("running the per-day driver: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "OK" {
		t.Errorf("per-day rate arithmetic is wrong:\n%s", got)
	}
}
