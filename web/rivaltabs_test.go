package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Against the real views.js, for the same reason the per-day driver is: a Go
// transcription of the ordering would pass while the shipped one picked the wrong
// teams, and picking the wrong teams is the entire failure mode here.
const rivalTabsDriver = `
import { rivalTeamTabs } from './views.mjs';

const t = (name, lifetime, last7d) =>
  ({ team_name: name, team_id: name.length, points_total: lifetime, points_last_7d: last7d });

// The donor this was found on: two of the four tabs were teams last folded for years
// ago, while the two folded for that week had none. Ordered by lifetime, "Team Pewds"
// and "USA" take slots from "Folding@Home Discord" and "FoldingStats.org".
const real = [
  t('University of Wisconsin-Madison', 2537325639, 95275573),
  t('Team Pewds',                      2381783463, 0),
  t('USA',                              745645066, 0),
  t("In Jesus' Name",                   197808451, 195634043),
  t('LoricaOS',                         186746425, 0),
  t('Folding@Home Discord',              85554960, 83253000),
  t('FoldingStats.org',                   5796477, 4127705),
];

const names = (rows) => rivalTeamTabs(rows).map((r) => r.team_name).join(' | ');

const cases = [
  ['active teams win the tabs', names(real),
   "In Jesus' Name | University of Wisconsin-Madison | Folding@Home Discord | FoldingStats.org"],

  // Every team dormant: nothing to rank on, so the incoming lifetime order stands
  // rather than the list collapsing to whatever the sort felt like.
  ['all dormant falls back to lifetime',
   names([t('Big', 900, 0), t('Middle', 500, 0), t('Small', 100, 0)]),
   'Big | Middle | Small'],

  // Ties are the common case for a wide donor, and a stable sort must keep the better
  // lifetime rank first instead of reshuffling between cycles for no reason.
  ['ties keep lifetime order',
   names([t('AAA', 900, 50), t('BBB', 500, 50), t('CCC', 100, 50)]),
   'AAA | BBB | CCC'],

  // A missing field must not sort above a real zero or throw.
  ['absent production sorts last',
   names([{ team_name: 'NoField', team_id: 1, points_total: 999 }, t('Active', 1, 5)]),
   'Active | NoField'],

  ['never more than four', String(rivalTeamTabs(real).length), '4'],
];

let bad = 0;
for (const [name, got, want] of cases) {
  if (got !== want) {
    console.log('FAIL ' + name + ':\n  got  ' + got + '\n  want ' + want);
    bad++;
  }
}
if (!bad) console.log('OK');
`

// TestRivalTeamTabs pins which four teams get a tab on a donor's Rivals card.
//
// Ordering by lifetime points reads as obviously right and is not: a donor's biggest
// teams by career total are routinely ones they left years ago, so the card offered
// competitions they are no longer in while the team they folded for that morning had no
// tab at all. Nothing errors when this is wrong — the card renders perfectly, against
// the wrong populations — which is why it is worth a test rather than an eyeball.
func TestRivalTeamTabs(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping rival tab ordering check")
	}

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
	if err := os.WriteFile(filepath.Join(dir, "driver.mjs"), []byte(rivalTabsDriver), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := exec.Command(node, filepath.Join(dir, "driver.mjs")).CombinedOutput()
	if err != nil {
		t.Fatalf("running the rival-tab driver: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "OK" {
		t.Errorf("rival team tabs are picking the wrong teams:\n%s", got)
	}
}
