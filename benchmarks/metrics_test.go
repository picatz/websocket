package benchmarks

import (
	"runtime/metrics"
	"slices"
	"testing"
	"time"
)

// latencySamples is how many of the most recent round trips a latencies
// keeps. It is allocated before timing starts, so recording allocates
// nothing.
const latencySamples = 1 << 16

// latencies records the duration of individual operations, so a benchmark
// can report percentiles alongside the mean that ns/op gives.
type latencies struct {
	samples []time.Duration
	n       int
}

func newLatencies() *latencies {
	return &latencies{samples: make([]time.Duration, latencySamples)}
}

// record stores one operation's duration, overwriting the oldest once full.
func (l *latencies) record(d time.Duration) {
	l.samples[l.n%len(l.samples)] = d
	l.n++
}

// report adds the 50th and 99th percentiles of the recorded operations to b.
func (l *latencies) report(b *testing.B) {
	s := l.samples[:min(l.n, len(l.samples))]
	if len(s) == 0 {
		return
	}
	slices.Sort(s)
	b.ReportMetric(float64(s[len(s)/2]), "p50-ns")
	b.ReportMetric(float64(s[len(s)*99/100]), "p99-ns")
}

// gcCost measures the CPU time the garbage collector spends over a
// benchmark. It leaves out the collector's idle-time marking, which only uses
// processors that would otherwise sit idle.
type gcCost struct {
	samples []metrics.Sample
	cpu     float64
}

var gcMetrics = []string{"/cpu/classes/gc/total:cpu-seconds", "/cpu/classes/gc/mark/idle:cpu-seconds"}

// startGCCost takes the starting reading. Call it before the timed loop.
func startGCCost() *gcCost {
	g := &gcCost{samples: make([]metrics.Sample, len(gcMetrics))}
	for i, name := range gcMetrics {
		g.samples[i].Name = name
	}
	g.cpu = g.read()
	return g
}

func (g *gcCost) read() float64 {
	metrics.Read(g.samples)
	return g.samples[0].Value.Float64() - g.samples[1].Value.Float64()
}

// report adds the collector's CPU time per operation to b, for ops
// operations since startGCCost. The runtime updates its estimate when a cycle
// ends, so a benchmark that allocates too little to start a cycle reports 0.
func (g *gcCost) report(b *testing.B, ops int) {
	if ops == 0 {
		return
	}
	b.ReportMetric((g.read()-g.cpu)*1e9/float64(ops), "gc-cpu-ns/op")
}
