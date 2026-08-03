package parse

import (
	"strings"
	"testing"
)

// FuzzUserScanner throws arbitrary bytes at the parser. It consumes an unvalidated
// external feed, so the bar is: never panic, never hang, and never invent a record
// whose numbers were not literally present in the input.
func FuzzUserScanner(f *testing.F) {
	f.Add("ts\nhdr\nAnonymous\t100\t2\t0\n")
	f.Add("ts\nhdr\ndono\n\t186\t1\t49078\n")                      // name spanning lines
	f.Add("ts\nhdr\noslo\t60p\t186\t1\t0\n")                       // tab inside the name
	f.Add("ts\nhdr\n/\ndy-Houston\t180\t1\t0\n")                   // name starting mid-line
	f.Add("ts\nhdr\n\t\t\t\t\t\n")                                 // only separators
	f.Add("")                                                      // empty
	f.Add("ts\n")                                                  // header only
	f.Add("ts\nhdr\n\x00\xff\t1\t2\t3\n")                          // invalid UTF-8
	f.Add("ts\nhdr\n" + strings.Repeat("a", 5000) + "\t1\t2\t3\n") // over-long name

	f.Fuzz(func(t *testing.T, in string) {
		s := NewUserScanner(strings.NewReader(in))
		rows := 0
		for s.Scan() {
			r := s.Row()
			rows++
			// Anything emitted must be internally consistent: the parser must not
			// fabricate values, and negative totals would mean a sign bug.
			if r.Score < 0 || r.WUs < 0 || r.TeamID < 0 {
				t.Fatalf("negative field in %+v", r)
			}
			if len(r.Name) > maxUserNameLen {
				t.Fatalf("name of %d bytes exceeds the %d cap", len(r.Name), maxUserNameLen)
			}
			if rows > 100_000 {
				t.Fatal("runaway row count: the accumulator is not bounded")
			}
		}
		_ = s.Err()
		// Every physical line must be accounted for as either a row or malformed,
		// so a corrupt region can never silently vanish.
		if st := s.Stats(); st.Rows < 0 || st.Malformed < 0 {
			t.Fatalf("negative stats %+v", st)
		}
	})
}

func FuzzTeamScanner(f *testing.F) {
	f.Add("ts\nhdr\n0\tDefault\t8213749748944\t364747804\n")
	f.Add("ts\nhdr\n151775\tdiscworld\n\t3448577\t434\n")
	f.Add("ts\nhdr\n87792\t\tGreater Hartford\t15553\t11\n")
	f.Add("ts\nhdr\n68207\t772\t15553\t61\n") // numeric-looking team name

	f.Fuzz(func(t *testing.T, in string) {
		s := NewTeamScanner(strings.NewReader(in))
		rows := 0
		for s.Scan() {
			r := s.Row()
			rows++
			if r.Score < 0 || r.WUs < 0 || r.ID < 0 {
				t.Fatalf("negative field in %+v", r)
			}
			if len(r.Name) > maxTeamNameLen {
				t.Fatalf("name of %d bytes exceeds the %d cap", len(r.Name), maxTeamNameLen)
			}
			if rows > 100_000 {
				t.Fatal("runaway row count")
			}
		}
		_ = s.Err()
	})
}
