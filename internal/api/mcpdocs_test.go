package api

import (
	"os"
	"regexp"
	"testing"
)

// TestDocumentedToolsMatchTheServedOnes closes the gap that opened the moment a second
// copy of the tool list existed.
//
// The /agents page hand-writes its table rather than rendering tools/list, because the
// descriptions the server ships are written to help a model choose and run to several
// sentences each — a wall of text for somebody skimming. That is a defensible reason to
// write them twice, but only with something holding the two together: four tools were
// added in one sitting and the page went on confidently documenting seven of eleven,
// which nothing else would ever have caught. The page does not fail, it just lies.
//
// Reading the JS as text rather than executing it is crude and entirely sufficient: the
// list is a literal, and what matters is the set of names in it.
func TestDocumentedToolsMatchTheServedOnes(t *testing.T) {
	src, err := os.ReadFile("../../web/views.js")
	if err != nil {
		t.Fatalf("cannot read the agents page: %v", err)
	}

	block := regexp.MustCompile(`(?s)const MCP_TOOLS = \[(.*?)\n\];`).FindSubmatch(src)
	if block == nil {
		t.Fatal("MCP_TOOLS is no longer a literal in views.js — this test needs updating, " +
			"or the page now renders tools/list and it can be deleted")
	}
	documented := map[string]bool{}
	for _, m := range regexp.MustCompile(`\[\s*'([a-z_]+)'`).FindAllSubmatch(block[1], -1) {
		documented[string(m[1])] = true
	}
	if len(documented) == 0 {
		t.Fatal("parsed no tool names out of MCP_TOOLS")
	}

	served := map[string]bool{}
	for _, tl := range mcpTools() {
		served[tl.Name] = true
	}

	for name := range served {
		if !documented[name] {
			t.Errorf("%s is served but missing from the table on /agents", name)
		}
	}
	for name := range documented {
		if !served[name] {
			t.Errorf("%s is documented on /agents but no longer served", name)
		}
	}

	// The page also states a count. Spelling it out is what went wrong the first time,
	// so this checks that no written-out number is sitting next to the tool list.
	if regexp.MustCompile(`(?i)it gets (one|two|three|four|five|six|seven|eight|nine|ten|eleven|twelve) tools`).Match(src) {
		t.Error("/agents spells out the tool count; derive it from MCP_TOOLS.length so it " +
			"cannot go stale")
	}
}
