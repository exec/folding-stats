package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Run against the shipped ui.js rather than a transcription of its arithmetic, for the
// same reason as the per-day driver: a copy of the formula in Go would pass while the
// one in the browser was wrong.
const statColsDriver = `
import { statColumns } from './ui.mjs';

// [tiles, columns that would have fitted, expected columns]
const cases = [
  // The case this exists for. Eight tiles in a five-wide space packed as 5 + 3 — a
  // full row with a stranded trio beneath it, which reads as a layout bug because in
  // every sense except the CSS it is one.
  ['eight in five',      8, 5, 4],
  // Already even, so it is left alone rather than evened into something worse.
  ['nine in five',       9, 5, 5],
  ['seven in five',      7, 5, 4],
  ['six in five',        6, 5, 3],
  // Everything fits on one row: no balancing to do.
  ['five in five',       5, 5, 5],
  ['four in five',       4, 5, 4],
  // Three rows balance too.
  ['eight in three',     8, 3, 3],
  ['eleven in five',    11, 5, 4],
  // Never more columns than would have fitted — that would overflow the container.
  ['nine in four',       9, 4, 3],
  ['two in five',        2, 5, 2],
  // A container too narrow for even one tile still gets a single column, not zero,
  // and never a division by zero.
  ['one column floor',   5, 0, 1],
  ['negative fit',       5, -3, 1],
  ['single tile',        1, 5, 1],
];

let bad = 0;
for (const [name, count, fits, want] of cases) {
  const got = statColumns(count, fits);
  if (got !== want) {
    console.log('FAIL ' + name + ': statColumns(' + count + ', ' + fits + ') = ' + got + ', want ' + want);
    bad++;
  }
  // Two invariants that hold for every input, whatever the expectations above say:
  // the answer is usable as a column count, and it never widens past what fits.
  if (!(got >= 1)) {
    console.log('FAIL ' + name + ': got a column count below one');
    bad++;
  }
  if (fits >= 1 && got > fits) {
    console.log('FAIL ' + name + ': ' + got + ' columns where only ' + fits + ' fit');
    bad++;
  }
}
if (!bad) console.log('OK');
`

// TestStatColumnsBalancesRows pins the column choice behind the stat grids.
//
// CSS packs auto-fit columns and lets the remainder fall onto the next row, which is
// right until the remainder is small: eight tiles in a five-column width become a row
// of five and a stranded trio. Grid cannot express "and balance the rows" because the
// choice depends on the item count, so the arithmetic lives in JS and is checked here.
func TestStatColumnsBalancesRows(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping stat column arithmetic check")
	}
	runJSDriver(t, node, statColsDriver)
}

// runJSDriver executes a driver module against the real embedded scripts.
//
// The modules reach each other by absolute path, which is how the browser resolves
// them and how node cannot — so the whole graph is copied out with its specifiers
// rewritten to .mjs siblings. Running the shipped code rather than a transcription of
// it is the point: a copy of the arithmetic in Go would pass while the browser's was
// wrong, which is the only failure these tests exist to catch.
func runJSDriver(t *testing.T, node, driver string) {
	t.Helper()
	dir := t.TempDir()
	names, err := scriptNames()
	if err != nil {
		t.Fatal(err)
	}
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
	if err := os.WriteFile(filepath.Join(dir, "driver.mjs"), []byte(driver), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, filepath.Join(dir, "driver.mjs")).CombinedOutput()
	if err != nil {
		t.Fatalf("running the driver: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "OK" {
		t.Errorf("driver reported:\n%s", got)
	}
}

// Run against the shipped charts.js. The failure this guards is invisible by
// construction — the bar is in the data and reachable by the tooltip, just drawn
// outside the plot — so there is nothing to eyeball and no error anywhere.
const xRangeDriver = `
import { xRange } from './charts.mjs';

// A stand-in for the uPlot instance: xRange only reads u.data[0].
const u = (xs) => ({ data: [xs] });
const run = (xs, gran) => {
  const f = xRange(gran);
  return f(u(xs), xs[0], xs[xs.length - 1]);
};

let bad = 0;
const check = (name, cond) => { if (!cond) { console.log('FAIL ' + name); bad++; } };

// Real hourly spacing from production: the publishes are ~3596s apart, not 3600.
const hourly = [0, 3596, 7192, 10788].map((s) => 1786000000 + s);
{
  const [min, max] = run(hourly, 'hourly');
  check('hourly keeps the left edge', min === hourly[0]);
  // The whole point: the last bucket gets a full bucket of room after it, or its bar
  // draws off the end of the plot and vanishes.
  check('hourly leaves room for the last bar', max === hourly[3] + 3596);
  check('hourly room is not the nominal hour', max !== hourly[3] + 3600);
}

// An irregular gap — a missed cycle — must not widen the trailing space. uPlot sizes
// bars from the smallest gap, so the room after the last one has to match that.
{
  const gappy = [0, 3596, 3596 + 7200, 3596 + 7200 + 3596].map((s) => 1786000000 + s);
  const [, max] = run(gappy, 'hourly');
  check('the smallest gap sets the room', max === gappy[3] + 3596);
}

// Degenerate inputs must still produce a usable range rather than NaN or a zero-width
// axis, which would take the whole chart down rather than one bar.
for (const [name, xs, gran, want] of [
  ['single point falls back to nominal', [1786000000], 'hourly', 3600],
  ['single point daily',                 [1786000000], 'daily', 86400],
  ['single point monthly',               [1786000000], 'monthly', 30 * 86400],
]) {
  const [min, max] = run(xs, gran);
  check(name, max === xs[0] + want && min === xs[0]);
}
{
  const f = xRange('hourly');
  const [min, max] = f({ data: [[]] }, 0, 0);
  check('empty data still yields a finite range', Number.isFinite(min) && Number.isFinite(max) && max > min);
}
if (!bad) console.log('OK');
`

// TestChartLeavesRoomForTheNewestBucket pins the x-scale padding on stacked charts.
//
// align: 1 draws a bar or step starting at its own timestamp and extending right, so
// with uPlot's default range of exactly [first, last] the newest bucket renders past
// the right edge and is clipped away. It is still in the series, so the tooltip finds
// it when the pointer moves past the last visible bar — the newest figure, readable
// but invisible, which is the one people are looking for.
func TestChartLeavesRoomForTheNewestBucket(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping chart range check")
	}
	runJSDriver(t, node, xRangeDriver)
}
