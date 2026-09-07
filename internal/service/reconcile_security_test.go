package service

import (
	"context"
	"fmt"
	"testing"
)

func TestIngestIsolatesForgedOverflow(t *testing.T) {
	for name, forged := range map[string]string{
		"score":            "forged\t9223372036854775807\t0\t32\nreal",
		"work units":       "forged\t0\t9223372036854775807\t32\nreal",
		"duplicate score":  "forged\t9223372036854775807\t0\t32\nforged\t9223372036854775807\t0\t32\nreal",
		"split work units": "forged\t0\t4611686018427387904\t32\nother\t0\t4611686018427387904\t999\nreal",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			a := seed(t, dir, nil)
			svc, srv, db := newService(t, dir, a)
			defer db.Close()
			for cycle := 1; cycle <= 4; cycle++ {
				rows := []string{
					fmt.Sprintf("real\t%d\t%d\t32", cycle*100, cycle*10),
					fmt.Sprintf("healthy\t%d\t%d\t51", cycle*100, cycle*10),
					fmt.Sprintf("healthy\t%d\t%d\t51", cycle*50, cycle*5),
					fmt.Sprintf("tab\tand\nnewline\t%d\t%d\t51", cycle*50, cycle*5),
				}
				if cycle == 2 || cycle == 3 {
					rows[0] = fmt.Sprintf("%s\t%d\t%d\t32", forged, cycle*100, cycle*10)
				}
				seed(t, dir, []world{{cyc(18 + cycle),
					[]string{fmt.Sprintf("32\tTarget\t%d\t%d", cycle*100, cycle*10),
						fmt.Sprintf("51\tHealthy\t%d\t%d", cycle*200, cycle*20)}, rows}})
				if n, err := svc.Ingest(context.Background()); err != nil || n != 1 {
					t.Fatalf("cycle %d: applied %d, error %v; unaffected data must advance", cycle, n, err)
				}
				if snap := srv.Current(); snap == nil || !snap.At.Equal(cyc(18+cycle)) {
					t.Fatalf("cycle %d was not published", cycle)
				}
				got := donorTotals(srv)
				if got["healthy"] != int64(cycle*150) || got["tab\tand\nnewline"] != int64(cycle*50) {
					t.Fatalf("healthy duplicate/delimiter names changed: %v", got)
				}
				want := int64(cycle * 100)
				if cycle == 2 || cycle == 3 {
					want = 100 // Quarantine preserves the last good member total.
				}
				if got["real"] != want {
					t.Fatalf("cycle %d target total = %d, want %d", cycle, got["real"], want)
				}
				if _, ok := got["forged"]; ok {
					t.Fatal("forged row reached published donor state")
				}
			}
		})
	}
}
