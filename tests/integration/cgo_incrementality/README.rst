cgo edit-loop observations
=========================

Run ``bazel test //tests/integration/cgo_incrementality:cgo_incrementality_test
--test_output=all`` to record the current incremental build behavior. The test
builds a mixed Go/C library, a Go consumer, and an executable. It measures a
clean build, a no-op build, cumulative body edits in ordinary Go, Go importing
``C``, separate C and C++ source files, a header, a Go dependency, and a
``cc_library`` dependency, plus shared preprocessor, C-only, and C++-only compiler
option changes, then another no-op build. All ten edits change the program's
result; exported Go functions are marked
``//go:noinline`` so ordinary
body edits need not change their callers' compiled code. It runs the same edit
sequence in ``baseline`` and ``split`` subtests; the latter enables
``--features=go_split_cgo``. Source files are restored between variants.
Every execution also checks exact ``runtime.Caller`` source paths and lines for
ordinary Go, nested cgo Go, and an authored relative ``//line`` directive, so
projection or temporary paths cannot silently replace source locations. The C++
function uses the C++ standard library, checking that its link flags are retained.

Each ``edit-loop`` JSON row in ``test.log`` names its variant and contains fixture
actions grouped by target and mnemonic, separate execution and disk/remote-cache
counts, per-source native compile input/output identities, output SHA-256 digests
(including package ``.a``, split ``.go.a``, export ``.x``, generated cgo files, and native objects),
and elapsed build time. A persistent local action-cache hit produces no
execution-log record. Elapsed time is informational, includes logging overhead, and is never
asserted. The clean build may still hit a configured disk or remote cache; its
row distinguishes those hits from execution. The ``links`` entries also preserve
Bazel's reported ``totalTime`` and ``executionWallTime`` protobuf duration
strings for each ``GoLink`` spawn, together with its runner and cache-hit state.
Missing timing fields remain omitted rather than being reported as measured zero.

In the baseline, ``GoCompilePkg`` is a composite action that runs cgo, native
compilers, and the Go compiler. Its ``GoLink`` counterpart can invoke the native
linker. No ``CppCompile`` entry does **not** mean no C compiler ran. New Bazel-visible
stages appear automatically in the action counts. The split variant records
``GoCgoProject``, ``GoCgoGenerate``, one ``GoCgoCompile`` per original native source,
and ``GoCgoPrepare``. Generation still groups cgo and compilation of generated C
code; preparation performs the native link probe and dynamic-import generation.
Raw execution logs are temporary; compact rows remain in test.log.

The regression gates check that the edited program behaves correctly, its
edited target's archive changes, and no-op builds preserve output bytes and skip
all fixture spawns. The split variant additionally requires that an ordinary Go
or Go dependency body edit skip ``GoCgoPrepare`` entirely and preserve its output
bytes. Edits to Go importing ``C``, C, a header, a native dependency, or native
compiler options execute preparation or retrieve its result from a disk/remote
cache. All of these except the separately linked native dependency must also
change prepared output bytes. Each C or C++ body edit must refresh exactly its own
``GoCgoCompile``, preserve the other source's object bytes, and skip generation.
A shared header or preprocessor-option edit must refresh both native compiles.
C-only options refresh generation and only the C compile; C++-only options skip
generation and refresh only the C++ compile. The other object's bytes stay stable.
A cgo Go body edit refreshes generation but preserves the native compiles when
their generated header is unchanged. A native dependency body edit skips both
generation and native compilation. Ordinary Go and Go dependency edits also skip
generation and native compilation.

Split packages also expose a Go archive without the cgo native objects. C, C++,
and native dependency body edits in this fixture must preserve its bytes;
ordinary Go and cgo Go body edits must change them. These checks establish a
cache boundary for a future Go prelink action. The conventional archive and
final executable still change after effective native edits. Dynamic-import
metadata remains in the Go archive: native edits that change that metadata may
also invalidate it. Go compilation and final linking are not skipped by this
artifact separation alone.

A separate retention fixture calls both Go assembly and code from a ``.syso``
file. With no cgo native members, its full archive and Go-only archive must be
byte-identical. This guards against classifying every ``.o`` member as native
or dropping system object members while introducing the new boundary.

Final linking remains visible as repeated ``GoLink`` execution/cache counts;
this optimization does not promise to skip final linking after effective code
edits. Its reported timings cover the entire builder action, including Go and
native linker subprocesses. Bazel's execution log cannot separate their work,
so these rows do not claim native-link subprocess counts or individual timings.
The gates do not freeze other action counts or export-file invalidation behavior.
Additional build flags can be supplied using the existing
``GO_BAZEL_TEST_BAZELFLAGS`` test environment variable.
