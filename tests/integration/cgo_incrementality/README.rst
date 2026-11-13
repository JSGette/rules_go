cgo edit-loop observations
=========================

Run ``bazel test //tests/integration/cgo_incrementality:cgo_incrementality_test
--test_output=all`` to record the current incremental build behavior. The test
builds a mixed Go/C library, a Go consumer, and an executable. It measures a
clean build, a no-op build, cumulative body edits in ordinary Go, Go importing
``C``, C, a header, a Go dependency, and a ``cc_library`` dependency, plus a native
compiler-option change, then another no-op build. All seven edits change the
program's result; exported Go functions are marked ``//go:noinline`` so ordinary
body edits need not change their callers' compiled code. It runs the same edit
sequence in ``baseline`` and ``split`` subtests; the latter enables
``--features=go_split_cgo``. Source files are restored between variants.
Every execution also checks exact ``runtime.Caller`` source paths and lines for
ordinary Go, nested cgo Go, and an authored relative ``//line`` directive, so
projection or temporary paths cannot silently replace source locations.

Each ``edit-loop`` JSON row in ``test.log`` names its variant and contains fixture
actions grouped by target and mnemonic, separate execution and disk/remote-cache
counts, output SHA-256 digests (including package ``.a``, export ``.x``, and
prepared cgo files), and elapsed
build time. A persistent local action-cache hit produces no execution-log
record. Elapsed time is informational, includes logging overhead, and is never
asserted. The clean build may still hit a configured disk or remote cache; its
row distinguishes those hits from execution.

In the baseline, ``GoCompilePkg`` is a composite action that runs cgo, native
compilers, and the Go compiler. Its ``GoLink`` counterpart can invoke the native
linker.
No ``CppCompile`` entry does **not** mean no C compiler ran. New Bazel-visible
stages appear automatically in the action counts. The split variant records
``GoCgoProject`` and ``GoCgoPrepare``; preparation still contains native compiler
subprocesses. Raw execution logs are temporary; compact rows remain in test.log.

The regression gates check that the edited program behaves correctly, its
edited target's archive changes, and no-op builds preserve output bytes and skip
all fixture spawns. The split variant additionally requires that an ordinary Go
or Go dependency body edit skip ``GoCgoPrepare`` entirely and preserve its output
bytes. Edits to Go importing ``C``, C, a header, a native dependency, or native
compiler options execute preparation or retrieve its result from a disk/remote
cache. All of these except the separately linked native dependency must also
change prepared output bytes.
The gates do not freeze other action counts or export file invalidation behavior.
Additional build flags can be supplied using the existing
``GO_BAZEL_TEST_BAZELFLAGS`` test environment variable.
