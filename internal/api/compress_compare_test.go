package api

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"testing"

	fastgzip "github.com/klauspost/compress/gzip"
)

func BenchmarkGzipComparison(b *testing.B) {
	for _, count := range []int{10, 100, 1000} {
		rows := make([]Team, count)
		for i := range rows {
			rows[i] = Team{TeamID: int32(i), Name: fmt.Sprintf("Folding team %d", i), Rank: int32(i + 1),
				Production: Production{PointsTotal: int64(i+1) * 987654321, WUsTotal: int64(i+1) * 12345,
					PointsLast24h: int64(i+1) * 2468, PointsPerDay7dAvg: int64(i+1) * 1234}}
		}
		body, err := json.Marshal(rows)
		if err != nil {
			b.Fatal(err)
		}
		for _, fast := range []bool{false, true} {
			b.Run(fmt.Sprintf("rows=%d/fast=%v", count, fast), func(b *testing.B) {
				var zw interface {
					io.WriteCloser
					Reset(io.Writer)
				}
				if fast {
					zw, _ = fastgzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
				} else {
					zw, _ = gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
				}
				var out bytes.Buffer
				b.ReportAllocs()
				b.SetBytes(int64(len(body)))
				for b.Loop() {
					out.Reset()
					zw.Reset(&out)
					if _, err := zw.Write(body); err != nil {
						b.Fatal(err)
					}
					if err := zw.Close(); err != nil {
						b.Fatal(err)
					}
				}
				b.ReportMetric(float64(out.Len()), "compressed-B")
			})
		}
	}
}
