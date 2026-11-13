Link metadata inputs
====================

Run ``bazel test //tests/integration/link_metadata:link_metadata_test`` to check
that changing main and dependency package metadata does not relink targets in
``c-archive``, ``c-shared``, or ``plugin`` mode. These modes do not embed Go build
information. The test checks execution logs and byte-identical outputs, with an
executable control that must relink and report the changed module versions via
``runtime/debug.ReadBuildInfo``. Plugin mode is omitted on Windows.

For normal executables, metadata and imports manifests belonging only to
unimported dependencies are recorded in Bazel's ``unused_inputs_list``. The
test changes unused metadata and checks that the executable is reused without a
link spawn. It also checks that shared metadata stays an input when a reachable
package uses it, and that adding an import restores the previously unused
metadata as an input and embeds its current module version.
