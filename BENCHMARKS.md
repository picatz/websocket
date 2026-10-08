# Performance measurements

These benchmarks separate allocation and framing costs from transport costs.
They are diagnostic tools, not a claim of production capacity or a comparison
with other WebSocket libraries.

## Reproduce

Record the exact commit, Go version, GOOS/GOARCH/GOAMD64, CPU, GOMAXPROCS, and
whether other workloads share the machine. Run correctness checks first:

```sh
go test ./...
go test -race ./...
GOARCH=386 CGO_ENABLED=0 go test ./...
go test -run '^$' -fuzz '^FuzzMask$' -fuzztime=30s -parallel=2
go test -run '^$' -fuzz '^FuzzReadMessage$' -fuzztime=30s -parallel=2
```

For stable samples, use an otherwise quiet machine. Keep compiler version,
configuration, benchmark harness, and hardware identical before and after a
change. Interleave before/after runs to reduce time-of-day or thermal drift.

```sh
go test -run '^$' -bench '^BenchmarkMask$' -benchmem -benchtime=1s -count=10 -cpu=1
go test -run '^$' -bench '^Benchmark(ReadFrame|ReadMessage|WriteMessage)$' -benchmem -benchtime=1s -count=10 -cpu=1
go test -run '^$' -bench '^BenchmarkTransport$' -benchmem -benchtime=1s -count=10 -cpu=2
```

Filter sub-benchmarks for focused investigations rather than repeatedly running
the entire matrix. Compare raw repeated samples with Go's
[benchstat](https://pkg.go.dev/golang.org/x/perf/cmd/benchstat); report the sample
count, confidence intervals, and nonsignificant results. A single run is useful
for finding allocation counts but is insufficient evidence for a speed claim.

Profile a selected workload in a separate run; do not compare instrumented
numbers with uninstrumented numbers:

```sh
go test -run '^$' -bench '^BenchmarkReadMessage/bytes=65536/masked=true/fragments=1$' -benchtime=5s -cpuprofile=cpu.out -memprofile=mem.out
go tool pprof -top cpu.out
go tool pprof -alloc_space -top mem.out
```

## What each benchmark measures

- `Mask`: cache-hot, in-place masking of one reused buffer, with equal indirect
  function-call overhead for the implementation variants. Offset 1 exercises an
  unaligned address. Setup and key generation are excluded. This isolates the
  masking routine; it does not predict whole-message throughput. The scalar
  reference directly applies RFC 6455's four-byte cycle. The test-only word
  candidate uses portable `encoding/binary` operations and no assembly/unsafe.
  The `stdlib` candidate expands the key into 1 KiB of stack scratch and calls
  `crypto/subtle.XORBytes`, which uses architecture-optimized code on supported
  platforms. Expansion cost is measured on every call; smaller inputs use the
  word candidate. This is one bounded experiment, not an exhaustive SIMD search.
- `ReadFrame`: one parsed frame from a reset in-memory reader, including its
  payload allocation and optional unmasking. The reader-reset overhead is part
  of each iteration.
- `ReadMessage`: the same memory source, with complete-message validation and
  assembly. It covers one or four frames, both masking directions, short and
  extended-length headers, and payload sizes up to 1 MiB. Independent fixtures
  and unit tests check their exact decoded bytes.
- `WriteMessage`: framing, optional random mask generation, allocation, and
  masking on a reused connection writing to `io.Discard`. In particular,
  server/unmasked output can bypass payload copying and the sink does not touch
  its bytes. Reported MB/s is logical payload bytes per second, **not** network
  throughput or memory bandwidth. One untimed write initializes crypto/rand.
- `Transport`: sustained one-way message delivery between two reused endpoints,
  with framing, random masking, receiver allocation, and message assembly. It is
  not request/response or round-trip latency. `pipe` is synchronous in-memory
  handoff; `tcp` adds loopback sockets and kernel scheduling. Allocation metrics
  include both goroutines. Handshake, TLS, compression, connection churn,
  competing streams, and real-network delays are excluded. Both endpoints have
  a two-minute per-sample deadline; keep benchtime below this bound.

`B/op` reports cumulative heap bytes allocated per operation, not resident
memory, peak live heap, or retained connection memory. The mask routine should
remain allocation-free; low-level masking gains do not reduce the read path's
payload allocation or copying.

## Optimization decisions

Prefer a simple portable implementation when it supplies a material measured
win. Keep small-payload regressions visible rather than quoting only the largest
buffer. Architecture-specific SIMD would need independent end-to-end evidence,
CPU-feature dispatch, a portable fallback, and validation on each supported
architecture. A faster isolated loop alone does not justify that maintenance
cost. No custom assembly is part of this benchmark harness. The test-only `stdlib`
candidate reuses the standard library's optimized implementation. Zero heap
allocations do not mean zero stack cost.

References: [Go testing](https://pkg.go.dev/testing),
[Go profiling](https://go.dev/blog/pprof), and
[RFC 6455 masking](https://www.rfc-editor.org/rfc/rfc6455.html#section-5.3).

## Diagnostic snapshot: 2026-10-08

This snapshot compares production `cfa54a56ea737b0dc9af2d44a0b02894fcd19931`
with a local prototype that changes only `xor` to the portable word candidate.
The benchmark harness is `98ac0afb64abaa9b701e17dde95cacffaad9c87b`.
It does not describe later changes to message reading or establish deployment
capacity. Re-measure the complete paths when those implementations change.

Environment: Go 1.27.1, linux/amd64, GOAMD64=v1, Intel Xeon Platinum 8573C on
shared cloud hardware. Known competing local jobs were stopped. Mask/read/write
runs used CPU affinity 4 and GOMAXPROCS=1; transport used CPUs 4–5 and
GOMAXPROCS=2. There were ten samples per case: 150 ms for memory-source/framing
benchmarks and 200 ms for transport. Before/after order alternated between
pairs. Mask variants were interleaved across ten complete sweeps. An initial
unpinned sweep showed severe drift and was excluded from these comparisons.

Representative observed median times (microseconds per operation):

| Workload | Scalar baseline | Word prototype | Interpretation |
| --- | ---: | ---: | --- |
| 4 KiB masked ReadMessage, one frame | 12.31 | 7.87 | Lower in-memory processing time |
| 64 KiB masked ReadMessage, four frames | 185.47 | 139.69 | Lower in-memory processing time |
| 4 KiB client WriteMessage to discard | 6.82 | 2.90 | Lower framing/masking time |
| 64 KiB client WriteMessage to discard | 108.37 | 32.07 | Lower framing/masking time |
| 16-byte client WriteMessage to discard | 0.275 | 0.288 | No demonstrated improvement |
| 64 KiB unmasked ReadMessage, one frame | 68.55 | 89.14 | Unchanged-code control; noisy |
| 64 KiB client-to-server loopback TCP | 366.14 | 427.20 | No demonstrated improvement |

These are workload-specific observations, not guaranteed speedups. Transport
measurements and some unchanged-code controls were noisy even after pinning.
All six TCP medians worsened (roughly 5–25%), and the 16-byte masked
net.Pipe median worsened by 22%. Transport results are inconclusive; regressions
cannot be ruled out, so this does not establish a general latency improvement. For example, the distribution-free
median interval for the 64 KiB TCP case was 303.91–509.10 µs before versus
247.91–1383.55 µs after. For the 64 KiB discard write it was 89.35–126.35 µs
before versus 24.11–49.15 µs after. Ten samples give these order-statistic
intervals 97.85% nominal coverage under independent sampling; shared-host drift
and temporal dependence can weaken that interpretation. Exploratory rank-test p-values in the raw summary
are unadjusted for multiple comparisons; do not interpret them as a release
performance guarantee.

Heap allocation did not improve: a 64 KiB single-frame ReadMessage still
allocated 203,736 B in 21 allocations; four fragments used 300,848 B in 71
allocations. Reading the same single frame without assembling a message used
138,200 B in 20 allocations. The extra complete-message copy accounts for
exactly one payload-sized allocation. The 1 MiB message case used 3,276,632 B
in 29 allocations. These are cumulative allocation totals, not retained memory.

Separate five-second profiles support the identified hotspot: in the 64 KiB
client discard-write workload, masking accounted for approximately 69% of flat
CPU samples before and 28% after. In the masked read workload, masking fell
from approximately 43% to 9% of flat CPU samples. In the latter allocation
profile, io.ReadAll accounted for roughly 68% of allocated bytes and complete
message assembly for 32%. Allocation/copying is a separate next optimization
problem; any change must retain bounded reads for untrusted declared lengths.

### SIMD decision

For an aligned (offset 0), cache-hot 64 KiB buffer, median isolated mask times were
80.44 µs (scalar production), 9.73 µs (portable word), and 4.42 µs (stdlib
XORBytes candidate). At 1 MiB they were 1305.26, 166.47, and 81.82 µs.
For offset 1, scalar versus word masking was 80.99 versus 10.56 µs at 64 KiB.
The stdlib experiment therefore shows potential additional large-buffer gains.
At 1025 bytes, however, the word candidate took 0.169 µs versus 0.192 µs for
the stdlib candidate. The portable word loop also adds about 1–2 ns at tiny
0–7-byte sizes in this indirect-call harness; that tradeoff is not hidden.

All mask variants allocated zero heap bytes. Compiler inspection showed that
the stdlib experiment reserves a 1,120-byte stack frame on amd64, including
its small-input fallback, versus no local scratch buffer for the word loop.
No full-path stdlib-candidate improvement was measured, and no ARM64 runtime
measurements were made. The simpler portable word loop is the candidate for further validation, but
transport regression uncertainty must be resolved before shipping it. A
production SIMD path needs its own complete-message proof and architecture/stack
cost assessment; this snapshot does not justify custom assembly or an
application-visible SIMD option.
