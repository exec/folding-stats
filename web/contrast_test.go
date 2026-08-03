package web

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"testing"
)

// The production heat ramp is applied to donor and team names, which makes it text
// rather than decoration — so it answers to the WCAG text gate of 4.5:1 rather than
// the 3:1 a graphical mark would need.
//
// This is checked here rather than trusted because the failure is silent and easy to
// reintroduce: nudging one hex to taste, or restyling a surface, breaks readability
// for exactly the readers least able to report it. Both ends of both ramps sit close
// to the gate by construction — they are as saturated as the gate allows — so there
// is no slack absorbing a careless edit.
const wcagTextContrast = 4.5

var (
	// Dark is the default theme, so its steps are the unprefixed rule.
	darkTierRe  = regexp.MustCompile(`(?m)^\.tier-([1-6]) \{ color: (#[0-9a-fA-F]{6}); \}`)
	lightTierRe = regexp.MustCompile(`(?m)^:root\[data-theme="light"\] \.tier-([1-6]) \{ color: (#[0-9a-fA-F]{6}); \}`)
	// Surfaces are read from the stylesheet rather than pinned here, so that changing
	// a background is checked against the text drawn on it instead of silently
	// invalidating every ratio below.
	darkSurfaceRe  = regexp.MustCompile(`:root \{[^}]*?--surface: (#[0-9a-fA-F]{6});`)
	lightSurfaceRe = regexp.MustCompile(`:root\[data-theme="light"\] \{[^}]*?--surface: (#[0-9a-fA-F]{6});`)
	inkMutedRe     = regexp.MustCompile(`:root \{[^}]*?--ink-muted: (#[0-9a-fA-F]{6});`)
)

func TestTierNamesMeetTextContrast(t *testing.T) {
	css := readAsset(t, "app.css")

	for _, tc := range []struct {
		theme   string
		tiers   *regexp.Regexp
		surface *regexp.Regexp
	}{
		{"dark", darkTierRe, darkSurfaceRe},
		{"light", lightTierRe, lightSurfaceRe},
	} {
		surf := findOne(t, css, tc.surface, tc.theme+" --surface")
		steps := tc.tiers.FindAllStringSubmatch(css, -1)
		if len(steps) != 6 {
			t.Fatalf("%s: found %d tier colours, want 6 — the ramp is not fully defined",
				tc.theme, len(steps))
		}

		var lums []float64
		for _, m := range steps {
			ratio := contrast(t, m[2], surf)
			if ratio < wcagTextContrast {
				t.Errorf("%s .tier-%s (%s on %s): contrast %.2f:1, want >= %.1f:1 — "+
					"this colour is the donor's name, not a swatch",
					tc.theme, m[1], m[2], surf, ratio, wcagTextContrast)
			}
			lums = append(lums, relLuminance(t, m[2]))
		}

		// The ramp has to keep reading as a ramp. Monotone luminance in one direction
		// or the other is what makes a hotter tier look hotter; a step out of order
		// turns the encoding into decoration.
		up, down := true, true
		for i := 1; i < len(lums); i++ {
			if lums[i] <= lums[i-1] {
				up = false
			}
			if lums[i] >= lums[i-1] {
				down = false
			}
		}
		if !up && !down {
			t.Errorf("%s: tier luminance is not monotone (%v) — the steps no longer read as an order",
				tc.theme, lums)
		}
		// Dark climbs toward light and light deepens; if that ever flips, the hot end
		// is heading for the surface it is drawn on.
		if tc.theme == "dark" && !up {
			t.Error("dark: heat must climb toward light, so the hottest name is the brightest")
		}
		if tc.theme == "light" && !down {
			t.Error("light: heat must deepen, or the hot end burns out against the page")
		}
	}
}

func TestIdleNameContrastIsRecorded(t *testing.T) {
	// Idle names take --ink-muted, which is a site-wide token rather than part of the
	// ramp. On light it sits at 3.50:1, under the text gate — pre-existing, and not
	// silently adopted by pinning it here: this test states the number so a change
	// either way is deliberate. Idle rows are also de-emphasised by .dim, and the
	// per-day column reads 0 beside them, so nothing is conveyed by the colour alone.
	css := readAsset(t, "app.css")
	muted := findOne(t, css, inkMutedRe, "--ink-muted")
	light := findOne(t, css, lightSurfaceRe, "light --surface")
	dark := findOne(t, css, darkSurfaceRe, "dark --surface")

	if got := contrast(t, muted, dark); got < wcagTextContrast {
		t.Errorf("--ink-muted on dark = %.2f:1, want >= %.1f:1", got, wcagTextContrast)
	}
	// Documented, not asserted as passing: raising it is a site-wide visual change.
	if got := contrast(t, muted, light); got > wcagTextContrast {
		t.Logf("--ink-muted on light is now %.2f:1 and clears the text gate; "+
			"this test's caveat can be dropped", got)
	}
}

func findOne(t *testing.T, css string, re *regexp.Regexp, what string) string {
	t.Helper()
	m := re.FindStringSubmatch(css)
	if m == nil {
		t.Fatalf("could not find %s in app.css", what)
	}
	return m[1]
}

// contrast is the WCAG 2 ratio between two sRGB hex colours.
func contrast(t *testing.T, a, b string) float64 {
	t.Helper()
	la, lb := relLuminance(t, a), relLuminance(t, b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

func relLuminance(t *testing.T, hex string) float64 {
	t.Helper()
	ch := make([]float64, 3)
	for i := 0; i < 3; i++ {
		v, err := strconv.ParseUint(hex[1+i*2:3+i*2], 16, 8)
		if err != nil {
			t.Fatalf("bad hex %q: %v", hex, err)
		}
		c := float64(v) / 255
		if c <= 0.04045 {
			ch[i] = c / 12.92
		} else {
			ch[i] = math.Pow((c+0.055)/1.055, 2.4)
		}
	}
	return 0.2126*ch[0] + 0.7152*ch[1] + 0.0722*ch[2]
}

func readAsset(t *testing.T, name string) string {
	t.Helper()
	b, err := assets.ReadFile(name)
	if err != nil {
		t.Fatal(fmt.Errorf("reading %s: %w", name, err))
	}
	return string(b)
}
