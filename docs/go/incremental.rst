Incremental Go build work
=========================

This fork develops incremental build boundaries that Bazel can cache and send
to remote executors. The default rules retain their existing behavior. The
``go_split_cgo`` feature enables the current experiment with the configured
C/C++ toolchain; it does not require a replacement Go compiler.

Milestones and acceptance gates
-------------------------------

1. **Reproducibility and observations — implemented.** Preserve source paths
   through coverage and cgo projection. Record action counts, artifact digests,
   runtime results, and Bazel link-action durations for a fixed edit sequence.
   Compare both combined and split builds; do not use timing thresholds as
   correctness gates.

2. **Independent native compilation — implemented.** Project files importing
   ``C`` separately from ordinary Go files. Generate wrappers once, then compile
   original native sources in individual actions. An ordinary Go edit reuses
   native work; a C/C++ body edit recompiles only its own original object.
   Header and language-specific option edits invalidate their consumers.

3. **Go archive and native object separation — implemented in this branch.**
   Emit ``.go.a`` without cgo native members and retain the conventional ``.a``.
   Keep Go assembly and ``.syso`` members in both. Carry the Go archive and
   ordered native sidecar through private archive data. The edit-loop gate
   requires stable Go archive bytes after C/C++ and native dependency body
   edits, and different bytes after Go body edits. Default linking still uses
   the conventional archive. Milestone 5 removes its packing dependency from
   Go compilation.

4. **Cache Go prelink independently — opt-in implementation in this branch.**
   ``--features=go_split_link`` separates ``GoPrelink`` from ``GoHostLink`` for
   Go 1.26.7 or 1.26.10 Linux amd64 normal executables. The first produces ``go.o`` and
   a versioned host-link plan; the second consumes native sidecars, ``cdeps``,
   and that plan. The standalone prototype under ``experiments/go_prelink``
   remains as a small stock-Go demonstration. The Bazel implementation stores
   path tokens in its plan so that replay can use a different execution root.

   Integration gates: select sidecars through actual import reachability,
   including blank imports; omit declared-but-unimported packages; preserve
   callbacks, constructors, library ordering, runtime metadata, FIPS
   finalization, build IDs, debug information, and executable results. A native
   body edit must run host linking with no Go prelink spawn/cache lookup.
   A Go body edit must refresh both stages. The integration fixture requires a
   prelink disk-cache hit and real host-link execution in a second output base
   after a native edit. Remote execution service validation remains outstanding.
   Forwarded file-reading linker options are rejected until capability probing
   can model their additional dependencies correctly.

5. **Remove residual Go compilation after native edits — opt-in implementation.**
   ``GoCompilePkg`` produces the Go-only archive, export data, and archive
   boundary metadata. ``GoPack`` inserts the ordered native members before
   retained Go assembly/``.syso`` members to produce the compatibility archive.
   Native source and object bytes are absent from the compiler inputs.
   ``GoCgoProject`` preserves execution-time selection and projects assembly
   and headers only when they belong to the Go assembler; a separate Go-source
   manifest avoids reading the native sidecar. Nogo and coverage fallback
   retain their existing behavior.

   Integration gates require native body edits to skip Go compilation and,
   with split linking, Go prelink entirely. A fresh execution root must restore
   the compilation from cache after a native edit. Dynamic-import changes may
   still invalidate Go compilation; an external-only path can later avoid the
   cgo probe's dynamic-import dependency. The edit-loop fixture records action
   counts and clean-build elapsed time, not an isolated measurement of copy
   overhead; timings are not used as correctness thresholds.
   Representative performance and remote execution validation remain necessary
   before enabling this by default.

6. **Reuse compiler backend work — separate compiler experiment if needed.**
   Keep package-level Bazel actions and first cache code generation per function
   or dependency group after normal parsing, type checking, inlining, escape
   analysis, and walk, before ``ssagen.Compile``. Cache relocatable results with
   their GC/stack/PC/debug metadata, not only machine-code bytes. Key entries on
   lowered IR, compiler and target configuration, ABI/layout facts, optimization
   and PGO inputs, source positions, and relevant imported summaries. Retain
   full dependency invalidation for changes to inlining, escape results,
   generic instantiations, and type layout. Compare cold and warm compiler
   output and runtime behavior before attempting incremental type checking.
   Worker state must be disposable; sharing backend entries remotely needs an
   explicit cache design beyond ordinary Bazel action caching. Any new compiler
   repository starts private.

7. **Reuse work across test binaries — a parallel track.** Build an explicit,
   immutable dependency image identified by SDK/configuration and the exact
   package closure. Consumers receive it through providers and declared inputs;
   they cannot silently substitute a different ABI or dependency version.
   Preserve separate test processes. Measure linker CPU, peak memory, output
   bytes, cache transfers, and test behavior separately. Host-linker incremental
   state can be an additional optimization where the configured toolchain
   supports it; the initial host-link action can perform a complete native link.

Small upstream candidates
------------------------

* ``codex/cgo-coverage-reproducibility`` fixes cgo/coverage source-path
  reproducibility independently of the split-action feature.
* ``codex/skip-unused-link-metadata`` omits build-info metadata inputs for modes
  that do not embed it: C archives, C shared libraries, and plugins.
* ``codex/prune-unused-link-metadata`` uses Bazel's ``unused_inputs_list`` for
  executable metadata belonging to packages outside the actual import closure.
  It retains shared metadata and real linker inputs, and tests import activation
  after a previously unused metadata file changes.
* The baseline edit-loop observations can be extracted from the experimental
  fixture as a standalone performance/regression harness. The native-action and
  prelink changes should remain opt-in until their platform and clean-build
  costs have been measured.

These branches are work in the fork, not submitted upstream changes.

Existing discussions and what they imply for the design
-------------------------------------------------------

The open `Go issue 82274 <https://github.com/golang/go/issues/82274>`_ describes
repeated linker work across large Bazel test closures and explicitly proposes
incremental linking and shared dependency images. Its description distinguishes
network transfer savings from linker CPU/memory savings. As checked on
2026-11-13, it had no maintainer design decision beyond routing to the compiler
team. It does not establish that either approach was rejected.

`rules_go issue 4550 <https://github.com/bazel-contrib/rules_go/issues/4550>`_
contains a shared-library proof of concept. The discussion identifies exact
toolchain, package version, and export-data matching as requirements, and
suggests a restricted provider interface instead of publishing general-purpose
shared outputs. That is a useful basis for milestone 7.

`Russ Cox's comment on Go issue 47788
<https://github.com/golang/go/issues/47788#issuecomment-954890659>`_ describes
problems with installation state in ``GOROOT``, shared-library cache behavior,
and assumptions embedded in consumer code. It proposes explicit shared-image
inputs and rejecting stale images. The proposal to remove shared mode was
declined; its closed issue status must not be read as evidence that removal
happened. Our design uses immutable declared artifacts and exact compatibility
keys to address those concerns.

For the cgo boundary, the `Go 1.26.7 cgo linking documentation
<https://github.com/golang/go/blob/go1.26.7/src/cmd/cgo/doc.go>`_ describes static
import/export directives and external linking. The `Go 1.26.7 FIPS linker code
<https://github.com/golang/go/blob/go1.26.7/src/cmd/link/internal/ld/fips140.go>`_
shows why external-link completion includes executable finalization. The
prototype preserves that step rather than treating the CC invocation as the
whole operation.

Go 1.26.10 is explicitly supported after comparing the official release sources:
`fips140.go <https://github.com/golang/go/blob/go1.26.10/src/cmd/link/internal/ld/fips140.go>`_
and ``main.go`` are unchanged from 1.26.7, and the only change in ``lib.go`` is a
Darwin host-link workaround. The builder, linker, and cached plan still require
an exact version match; this does not enable intermediate or future versions.
