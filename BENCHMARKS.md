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
cost. No SIMD implementation is part of this benchmark harness.

References: [Go testing](https://pkg.go.dev/testing),
[Go profiling](https://go.dev/blog/pprof), and
[RFC 6455 masking](https://www.rfc-editor.org/rfc/rfc6455.html#section-5.3).
