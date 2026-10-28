package objectstore

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

// TestSegmentRetention: the sweep deletes a log entry only once the newest snapshot covers it
// and it is older than the segment retention, and a snapshot only once it is neither one of
// the newest two nor younger than the retention. A fresh load still gets the whole state.
func TestSegmentRetention(t *testing.T) {
	r := newRig()
	s := r.open(t, "A")
	ctx := t.Context()
	day := 24 * time.Hour
	write := func(n int) {
		for range n {
			must(t, s.Create(ctx, newRun(fmt.Sprintf("r%02d", s.mem.Version()+1))))
		}
	}
	versions := func(prefix, suffix string) []uint64 {
		var out []uint64
		for _, n := range r.b.Names(prefix) {
			v, _ := versionOf(n, prefix, suffix)
			out = append(out, v)
		}
		return out
	}
	span := func(from, to uint64) []uint64 {
		var out []uint64
		for v := from; v <= to; v++ {
			out = append(out, v)
		}
		return out
	}
	write(6) // day 0: 1-6, snapshot 6
	must(t, s.Compact(ctx))
	r.clk.Advance(day)
	write(6) // day 1: 7-12, snapshot 12
	must(t, s.Compact(ctx))
	r.clk.Advance(day)
	write(6) // day 2: 13-18, snapshot 18, then 19-20 past it
	must(t, s.Compact(ctx))
	write(2)

	r.clk.Set(epoch.Add(7*day + time.Hour))
	_, err := s.Sweep(ctx)
	must(t, err)
	if got, want := versions(snapshotPrefix, snapshotSuffix), []uint64{12, 18}; !slices.Equal(got, want) {
		t.Fatalf("snapshots on day 7 = %v, want %v", got, want)
	}
	if got, want := versions(logPrefix, logSuffix), span(7, 20); !slices.Equal(got, want) {
		t.Fatalf("log on day 7 = %v, want %v (day 0's entries gone, the rest kept)", got, want)
	}

	r.clk.Set(epoch.Add(9*day + 2*time.Hour))
	_, err = s.Sweep(ctx)
	must(t, err)
	if got, want := versions(snapshotPrefix, snapshotSuffix), []uint64{12, 18}; !slices.Equal(got, want) {
		t.Fatalf("snapshots on day 9 = %v, want the newest two %v, however old", got, want)
	}
	if got, want := versions(logPrefix, logSuffix), []uint64{19, 20}; !slices.Equal(got, want) {
		t.Fatalf("log on day 9 = %v, want %v (what no snapshot covers stays)", got, want)
	}
	fresh := r.open(t, "fresh")
	sameRecords(t, fresh, s)
}
