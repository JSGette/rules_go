# Stock Go external-link handoff experiment

This experiment proves a restricted Go/native linking boundary with an unmodified
**Go 1.26.7 Linux amd64 SDK**, GCC, and an actual Linux amd64 host (including WSL).
It is not a compiler fork or a Bazel linking implementation.

Dependencies: Python 3, `gcc`, `nm`, and the specified Go SDK. No third-party
Python packages or downloaded Go modules are needed.

```sh
python3 probe.py \
  --sdk /path/to/go1.26.7 \
  --work-dir /path/to/probe-results \
  --cache-dir /dev/shm/go-prelink-probe-cache
```

The work and cache arguments are optional; each defaults to a fresh directory
under the system temporary directory. The script preserves its results and
prints their location. `--variant plain`, `--variant sha256`, or `--variant aes`
runs one case; the default runs all three.

The script performs these checks:

1. Build a cgo program whose C implementation returns `41`. A custom `-extld`
   delegates genuine compiler/linker capability probes to GCC. It recognizes the
   single final invocation containing the expected `go.o`, records its argument
   vector with exclusive creation, and deliberately exits with status `77`.
   The Go build must fail at that handoff, while producing a complete relocatable
   `go.o` and copied native objects. No executable is fabricated.
2. Replay the recorded invocation with GCC to create a real executable.
3. For the SHA-256 and AES cases, verify that the raw replay fails with
   `GODEBUG=fips140=on` because its FIPS verification checksum is missing.
4. Apply the Go 1.26.7 ELF FIPS finalization algorithm, then run the supported
   `go tool buildid -w` command. The executable must now run with FIPS checking
   enabled and return `41`.
5. Compile only a replacement native implementation returning `42`, substitute
   its object into the recorded invocation, finalize the new executable, and
   verify its behavior. The original `go.o` SHA-256 must remain unchanged, and
   the final Go build ID must change.
6. Compare behavior against an ordinary stock-Go rebuild of the edited source.

Build-time `GOFIPS140=latest` and an empty `GOEXPERIMENT` are explicit. Runtime
checks always use `GODEBUG=fips140=on`; inherited settings cannot silently disable
the negative FIPS test.

Each case writes a `result.json`, its capture receipt and invocation trace, and
the intermediate and executable artifacts. The run directory contains a combined
`summary.json`. This is functional equivalence and Go-object reuse evidence;
it does not claim byte-identical final binaries or a measured speedup.

The receipt deliberately retains local absolute paths. It is **not a relocatable
remote-cache manifest**, and the standalone environment is not a hermetic Bazel
action. A Bazel implementation still needs declared toolchain inputs, structured
path relocation, durable action outputs, archive/member provenance, response-file
validation, supported-platform gates, and its usual final build-ID handling.
The manual replacement keeps the native interface and link flags unchanged.

The FIPS helper adapts the algorithm in Go 1.26.7
`src/cmd/link/internal/ld/fips140.go` (`newFipsObj`, `addSection`, and `elffips`),
under the BSD license in `LICENSE.go.txt`. It calls no private Go API, but the ELF
metadata format is internal and is intentionally version-pinned. No support for
other SDK versions, architectures, operating systems, PIE/shared/plugin modes,
or different external toolchains is implied.
