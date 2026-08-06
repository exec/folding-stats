package bot

import (
	"os"
	"regexp"
	"testing"
)

// TestDocumentedCommandsMatchTheRegisteredOnes holds the /bots page to the bot.
//
// The page writes its own one-line descriptions rather than reusing the ones Discord
// shows, which are written to fit a command picker and read badly in a table. That is
// a fair reason to keep a second copy, but only with something tying the two together
// — the identical arrangement on /agents drifted the moment four tools were added, and
// the page went on documenting seven of eleven with nothing failing. A page that lists
// a command the bot does not register sends people to type something that does not
// exist; one that omits a command hides a feature completely.
//
// Reading the JS as text is crude and sufficient: the list is a literal, and the names
// are what matter. Command names in BOTS are the only array entries whose first element
// starts with a slash, which is what makes this pattern safe against the prose
// fragments in the same block.
func TestDocumentedCommandsMatchTheRegisteredOnes(t *testing.T) {
	src, err := os.ReadFile("../../web/views.js")
	if err != nil {
		t.Fatalf("cannot read the bots page: %v", err)
	}

	block := regexp.MustCompile(`(?s)const BOTS = \[(.*?)\n\];`).FindSubmatch(src)
	if block == nil {
		t.Fatal("BOTS is no longer a literal in views.js — this test needs updating, or " +
			"the page now renders the command list from somewhere authoritative and it " +
			"can be deleted")
	}
	documented := map[string]bool{}
	for _, m := range regexp.MustCompile(`\['(/[a-z]+)'`).FindAllSubmatch(block[1], -1) {
		documented[string(m[1])] = true
	}
	if len(documented) == 0 {
		t.Fatal("parsed no command names out of BOTS")
	}

	registered := map[string]bool{}
	for _, c := range Commands() {
		registered["/"+c.Name] = true
	}

	for name := range registered {
		if !documented[name] {
			t.Errorf("%s is registered with Discord but missing from the table on /bots", name)
		}
	}
	for name := range documented {
		if !registered[name] {
			t.Errorf("%s is listed on /bots but the bot does not register it", name)
		}
	}
}
