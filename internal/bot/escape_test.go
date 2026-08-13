package bot

import (
	"strings"
	"testing"
)

// The payloads from the 2026-08-13 audit, verbatim. Donor and team names are chosen by
// the public and arrive unescaped, so every Discord surface that renders one has to
// treat it as data rather than as the markup it is trying to be.
const (
	// Closes the bold span the warning and alert descriptions put it inside, then
	// renders a clickable link the bot appears to have authored.
	boldEscape = "A**\n[V](https://x.io)\n**"
	// Closes the fence /top wraps its table in, then reopens one before the bot's own
	// closing delimiter so the message still looks well-formed.
	fenceEscape = "A```\n[V](https://x.io)\n```"
)

func TestMdEscNeutralisesTheBoldEscape(t *testing.T) {
	got := mdEsc(boldEscape)
	for _, bad := range []string{"**", "[V]", "](", "https://x.io)"} {
		if strings.Contains(got, bad) {
			t.Errorf("mdEsc left %q live in %q", bad, got)
		}
	}
	// The name must still be readable, not deleted.
	if !strings.Contains(got, "A") || !strings.Contains(got, "V") {
		t.Errorf("mdEsc destroyed the name: %q", got)
	}
}

func TestCodeSafeCannotCloseAFence(t *testing.T) {
	got := codeSafe(fenceEscape)
	if strings.Contains(got, "`") {
		t.Errorf("codeSafe left a backtick, which can close the block: %q", got)
	}
	if strings.ContainsAny(got, "\n\r\t") {
		t.Errorf("codeSafe left a line break, which breaks table alignment: %q", got)
	}
	// mdEsc would be the wrong tool here: inside a fence its backslashes are printed
	// literally rather than escaping anything.
	if strings.Contains(codeSafe("plain name"), "\\") {
		t.Error("codeSafe is escaping rather than sanitising")
	}
}

// The end the reader actually sees: whatever a name contains, the assembled embed must
// hold exactly one preformatted region.
func TestTopBodyKeepsOneFence(t *testing.T) {
	var sb strings.Builder
	for i, name := range []string{"honest", fenceEscape, "also honest"} {
		sb.WriteString(strings.TrimRight(
			padded(i+1, trunc(codeSafe(name), 28), "1.2M"), " ") + "\n")
	}
	e := TextEmbed("Top", "https://foldingstats.org/donors", sb.String(), Snapshot{})

	// Exactly two delimiters means the block opened once and closed once, so nothing
	// in it was ever rendered as markdown.
	if n := strings.Count(e.Description, "```"); n != 2 {
		t.Errorf("description has %d fence delimiters, want exactly 2:\n%s", n, e.Description)
	}
	// The link syntax survives as visible text, and that is the point rather than a
	// miss: inside a fence it is inert, so the reader sees the name the participant
	// actually chose instead of a mangled one. What matters is where it sits.
	open := strings.Index(e.Description, "```")
	close := strings.LastIndex(e.Description, "```")
	at := strings.Index(e.Description, "](https://x.io)")
	if at < 0 {
		t.Fatal("the name's text was destroyed rather than neutralised")
	}
	if at < open || at > close {
		t.Errorf("link syntax landed outside the fence, where it renders:\n%s", e.Description)
	}
}

// Truncation must not be able to manufacture a closing fence, so hostile input is
// checked either side of the 28-byte boundary the leaderboard truncates at.
func TestTruncationCannotRebuildAFence(t *testing.T) {
	for _, n := range []int{26, 27, 28, 29, 30} {
		name := strings.Repeat("a", n) + "```"
		if got := trunc(codeSafe(name), 28); strings.Contains(got, "`") {
			t.Errorf("length %d produced a backtick after truncation: %q", n, got)
		}
	}
}

func padded(rank int, name, value string) string {
	return strings.Join([]string{itoa(rank) + ".", name, value}, " ")
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{byte('0' + i%10)}, b...)
	}
	return string(b)
}
