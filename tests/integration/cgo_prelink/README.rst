Go prelink reuse
===============

Run ``bazel test //tests/integration/cgo_prelink:cgo_prelink_test
--test_output=all`` on native Linux/amd64 with stock Go 1.26.7. Other hosts and
SDK versions skip this experimental fixture. The fixture exercises the
``go_split_cgo`` and ``go_split_link`` features.

The edit loop records Bazel execution-log observations for ``GoPrelink`` and
``GoHostLink``, distinguishing execution from disk/remote-cache hits. C, C++, and
native dependency body edits must skip prelink entirely, preserve ``go.o``, and
refresh only the host-link stage. A Go body edit refreshes both stages. Every
effective edit must change the executable and its content build ID; no-op builds
must preserve both artifacts and skip both link stages. Reported timing strings
come directly from Bazel and are diagnostic only.
Prelink inputs must exclude native sidecar trees, standalone C/C++ objects, and
native dependency objects and archives.

Every executable run enables ``GODEBUG=fips140=on``, checks a SHA256 result and
FIPS integrity verification, calls back from C into Go, and checks C++ and
blank-imported native constructors. A declared-but-unimported cgo dependency
defines a conflicting native symbol, detecting accidental inclusion of unused
native sidecars.

The relocation test uses two independent Bazel output bases with a shared
temporary disk cache. After the first build, the first output base is removed
and a native-only edit forces host linking in the second execroot while
requiring its prelink artifact to come from the cache. This verifies replay of
the captured link plan without access to its old execroot, rather than merely
retrieving a previously linked executable.
