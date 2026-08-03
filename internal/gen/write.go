package gen

import (
	"bufio"
	"io"
	"sort"
	"strconv"
	"time"

	"folding/internal/feed"
)

// feedTimeFormat matches upstream's leading timestamp line verbatim, e.g.
// "Sun Aug 02 21:29:05 GMT 2026".
const feedTimeFormat = "Mon Jan 02 15:04:05 GMT 2006"

// WriteTeams emits the team feed for the corpus's current state.
//
// Rows are ordered by score descending, as upstream's are. Nothing downstream
// depends on that ordering — but generating unsorted output would mean a latent
// dependency on it could never be caught here.
func (c *Corpus) WriteTeams(w io.Writer, at time.Time) error {
	order := make([]int32, len(c.Teams))
	for i := range order {
		order[i] = int32(i)
	}
	sort.Slice(order, func(a, b int) bool {
		return c.Teams[order[a]].Score > c.Teams[order[b]].Score
	})

	bw := bufio.NewWriterSize(w, 1<<20)
	bw.WriteString(at.UTC().Format(feedTimeFormat))
	bw.WriteString("\nteam\tteamname\tscore\twu\n")
	for _, i := range order {
		t := c.Teams[i]
		bw.WriteString(strconv.FormatInt(int64(t.ID), 10))
		bw.WriteByte('\t')
		bw.WriteString(t.Name)
		bw.WriteByte('\t')
		bw.WriteString(strconv.FormatInt(t.Score, 10))
		bw.WriteByte('\t')
		bw.WriteString(strconv.FormatInt(t.WUs, 10))
		bw.WriteByte('\n')
	}
	return bw.Flush()
}

// WriteUsers emits the user feed, ordered by score descending as upstream's is.
func (c *Corpus) WriteUsers(w io.Writer, at time.Time) error {
	order := make([]int32, len(c.Members))
	for i := range order {
		order[i] = int32(i)
	}
	sort.Slice(order, func(a, b int) bool {
		return c.Members[order[a]].Score > c.Members[order[b]].Score
	})

	bw := bufio.NewWriterSize(w, 1<<20)
	bw.WriteString(at.UTC().Format(feedTimeFormat))
	bw.WriteString("\nname\tscore\twu\tteam\n")
	for _, i := range order {
		m := c.Members[i]
		bw.WriteString(m.Name)
		bw.WriteByte('\t')
		bw.WriteString(strconv.FormatInt(m.Score, 10))
		bw.WriteByte('\t')
		bw.WriteString(strconv.FormatInt(m.WUs, 10))
		bw.WriteByte('\t')
		bw.WriteString(strconv.FormatInt(int64(m.TeamID), 10))
		bw.WriteByte('\n')
	}
	return bw.Flush()
}

// Publish writes one cycle into the archive, reproducing upstream's timing: the team
// feed a minute ahead of the user feed. That gap is not cosmetic — it is why the two
// files never reconcile exactly, and why ingest has to pair them by proximity.
func (c *Corpus) Publish(a *feed.Archive, at time.Time) error {
	teamAt := at.Add(-time.Minute)
	if err := publish(a, feed.Teams, teamAt, func(w io.Writer) error {
		return c.WriteTeams(w, teamAt)
	}); err != nil {
		return err
	}
	return publish(a, feed.Users, at, func(w io.Writer) error {
		return c.WriteUsers(w, at)
	})
}

func publish(a *feed.Archive, k feed.Kind, at time.Time, write func(io.Writer) error) error {
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(write(pw)) }()
	defer pr.Close()
	_, err := a.Put(k, at, pr)
	return err
}

// Generate advances the corpus and publishes n cycles ending at end, spaced by
// interval. Returns the timestamp of the first cycle written.
//
// Cycles are produced forward in time because the metrics windows reject
// out-of-order pushes, and because a backwards fill would give every donor a
// first-sighting delta at the wrong end of history.
func (c *Corpus) Generate(a *feed.Archive, end time.Time, n int, interval time.Duration,
	progress func(i int, at time.Time)) (time.Time, error) {

	start := end.Add(-time.Duration(n-1) * interval)
	for i := 0; i < n; i++ {
		at := start.Add(time.Duration(i) * interval)
		if i > 0 {
			c.Advance()
		}
		if err := c.Publish(a, at); err != nil {
			return start, err
		}
		if progress != nil {
			progress(i, at)
		}
	}
	return start, nil
}
