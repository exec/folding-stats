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

	dir := t.TempDir()
	names, err := scriptNames()
	if err != nil {
		t.Fatal(err)
	}
	// The modules reach each other by absolute path, which is how the browser resolves
	// them and how node cannot. Copy the graph and rewrite to the .mjs siblings.
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
	if err := os.WriteFile(filepath.Join(dir, "driver.mjs"), []byte(statColsDriver), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := exec.Command(node, filepath.Join(dir, "driver.mjs")).CombinedOutput()
	if err != nil {
		t.Fatalf("running the stat column driver: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "OK" {
		t.Errorf("stat column arithmetic:\n%s", got)
	}
}
