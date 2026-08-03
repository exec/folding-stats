// Package parse reads the upstream Folding@home summary feeds.
//
// The feeds are nominally tab-separated, but donor and team names are unescaped
// free text that can contain literal tabs and newlines. A strict field-count reader
// silently drops or corrupts those records. Instead this package anchors on the
// numeric columns, whose positions are fixed, and treats everything between them as
// the name:
//
//	team file: <id>  <name...>  <score>  <wu>
//	user file:        <name...>  <score>  <wu>  <team>
//
// A record is accumulated across physical lines until its numeric anchors parse,
// which recovers names containing newlines without special-casing them.
package parse

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const (
	// maxAccumulate caps how many physical lines may be joined into one record.
	// Real feeds resolve within two lines; the margin exists so a corrupt region
	// loses a few records instead of swallowing the rest of the file.
	maxAccumulate = 6

	// Name length bounds, measured against the 2026-08-02 corpus: the longest
	// real donor name is 73 bytes and the longest team name 146. These ceilings
	// sit well clear of legitimate data while stopping the accumulator from
	// absorbing a corrupt region into one enormous "name" — the failure mode that
	// makes garbage look like a valid record.
	maxUserNameLen = 128
	maxTeamNameLen = 256

	// maxNumericTail is the slack added to a name bound to get a whole-record
	// bound: three 19-digit integers plus separators, rounded up.
	maxNumericTail = 64
)

// TeamRow is one row of daily_team_summary.txt.
type TeamRow struct {
	ID    int32
	Name  string
	Score int64
	WUs   int64
}

// UserRow is one row of daily_user_summary.txt.
//
// Name is not a unique identifier: the same name occurs on many teams, and can even
// repeat within one team. The identifying pair is (Name, TeamID).
type UserRow struct {
	Name   string
	Score  int64
	WUs    int64
	TeamID int32
}

// Stats reports what a scan saw.
type Stats struct {
	// Timestamp is line 1 of the feed verbatim, e.g. "Sun Aug 02 21:29:05 GMT 2026".
	Timestamp string
	// Header is line 2, the column names.
	Header string
	// Rows successfully parsed.
	Rows int
	// Malformed physical lines discarded after failing to resolve into a record.
	Malformed int
}

// scanner is the shared record accumulator. Feed-specific parsing is supplied by
// the parse func, which returns false when the buffer is not yet a complete record.
type scanner[T any] struct {
	br      *bufio.Reader
	parse   func(fields []string) (T, bool)
	maxRec  int // bytes beyond which the buffer can never become a valid record
	pending []string
	cur     T
	stats   Stats
	err     error
	started bool
}

func newScanner[T any](r io.Reader, maxRec int, parse func([]string) (T, bool)) *scanner[T] {
	return &scanner[T]{
		// 1 MiB buffer: comfortably above any plausible line length, so ReadString
		// never has to stitch partial reads for well-formed input.
		br:     bufio.NewReaderSize(r, 1<<20),
		parse:  parse,
		maxRec: maxRec,
	}
}

// Scan advances to the next record, returning false at EOF or on error.
func (s *scanner[T]) Scan() bool {
	if s.err != nil {
		return false
	}
	if !s.started {
		s.started = true
		if !s.readPreamble() {
			return false
		}
	}

	for {
		line, err := s.br.ReadString('\n')
		atEOF := err == io.EOF
		if err != nil && !atEOF {
			s.err = err
			return false
		}
		line = strings.TrimSuffix(line, "\n")

		if line == "" && atEOF {
			// Trailing newline at EOF is not a record. Anything still pending
			// never resolved, so account for it.
			s.stats.Malformed += len(s.pending)
			s.pending = s.pending[:0]
			return false
		}

		s.pending = append(s.pending, line)
		for len(s.pending) > 0 {
			joined := strings.Join(s.pending, "\n")
			if rec, ok := s.parse(strings.Split(joined, "\t")); ok {
				s.cur = rec
				s.stats.Rows++
				s.pending = s.pending[:0]
				return true
			}
			// Appending another line can only make the name longer, so once the
			// buffer is over-long or over-deep, waiting cannot help. Drop from the
			// front to resync instead of letting a corrupt region grow unbounded.
			if len(joined) <= s.maxRec && len(s.pending) < maxAccumulate {
				break // still plausibly incomplete: pull another line
			}
			s.stats.Malformed++
			s.pending = s.pending[1:]
		}

		if atEOF {
			s.stats.Malformed += len(s.pending)
			s.pending = s.pending[:0]
			return false
		}
	}
}

// readPreamble consumes the timestamp and header lines.
func (s *scanner[T]) readPreamble() bool {
	ts, err := s.br.ReadString('\n')
	if err != nil && err != io.EOF {
		s.err = err
		return false
	}
	s.stats.Timestamp = strings.TrimRight(ts, "\r\n")

	hdr, err := s.br.ReadString('\n')
	if err != nil && err != io.EOF {
		s.err = err
		return false
	}
	s.stats.Header = strings.TrimRight(hdr, "\r\n")

	if s.stats.Timestamp == "" {
		s.err = fmt.Errorf("parse: empty feed")
		return false
	}
	return true
}

func (s *scanner[T]) Row() T       { return s.cur }
func (s *scanner[T]) Err() error   { return s.err }
func (s *scanner[T]) Stats() Stats { return s.stats }

// TeamScanner streams daily_team_summary.txt.
type TeamScanner struct{ *scanner[TeamRow] }

// NewTeamScanner returns a scanner over the team feed.
func NewTeamScanner(r io.Reader) *TeamScanner {
	return &TeamScanner{newScanner(r, maxTeamNameLen+maxNumericTail, parseTeam)}
}

// parseTeam anchors on <id> at the front and <score> <wu> at the back; the name is
// whatever lies between, tabs and all.
func parseTeam(f []string) (TeamRow, bool) {
	if len(f) < 4 {
		return TeamRow{}, false
	}
	id, ok := atoi32(f[0])
	if !ok {
		return TeamRow{}, false
	}
	score, ok := atoi64(f[len(f)-2])
	if !ok {
		return TeamRow{}, false
	}
	wus, ok := atoi64(f[len(f)-1])
	if !ok {
		return TeamRow{}, false
	}
	name := strings.Join(f[1:len(f)-2], "\t")
	if len(name) > maxTeamNameLen {
		return TeamRow{}, false
	}
	return TeamRow{ID: id, Name: name, Score: score, WUs: wus}, true
}

// UserScanner streams daily_user_summary.txt.
type UserScanner struct{ *scanner[UserRow] }

// NewUserScanner returns a scanner over the user feed.
func NewUserScanner(r io.Reader) *UserScanner {
	return &UserScanner{newScanner(r, maxUserNameLen+maxNumericTail, parseUser)}
}

// parseUser anchors on the trailing <score> <wu> <team>; everything before is the
// name. Names legitimately look numeric (e.g. "84036980"), so the leading field
// cannot be used to disambiguate.
func parseUser(f []string) (UserRow, bool) {
	if len(f) < 4 {
		return UserRow{}, false
	}
	score, ok := atoi64(f[len(f)-3])
	if !ok {
		return UserRow{}, false
	}
	wus, ok := atoi64(f[len(f)-2])
	if !ok {
		return UserRow{}, false
	}
	team, ok := atoi32(f[len(f)-1])
	if !ok {
		return UserRow{}, false
	}
	name := strings.Join(f[:len(f)-3], "\t")
	if len(name) > maxUserNameLen {
		return UserRow{}, false
	}
	return UserRow{Name: name, Score: score, WUs: wus, TeamID: team}, true
}

// atoi64 accepts only a clean non-negative integer. Being strict here is what makes
// the anchoring reliable: a lax parser would let name fragments masquerade as
// numeric columns and silently mis-split records.
func atoi64(s string) (int64, bool) {
	if s == "" || len(s) > 19 {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	v, err := strconv.ParseInt(s, 10, 64)
	return v, err == nil
}

func atoi32(s string) (int32, bool) {
	v, ok := atoi64(s)
	if !ok || v > 2147483647 {
		return 0, false
	}
	return int32(v), true
}
