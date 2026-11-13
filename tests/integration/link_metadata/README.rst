Link metadata inputs
====================

Run ``bazel test //tests/integration/link_metadata:link_metadata_test`` to check
that changing main and dependency package metadata does not relink targets in
``c-archive``, ``c-shared``, or ``plugin`` mode. These modes do not embed Go build
information. The test checks execution logs and byte-identical outputs, with an
executable control that must relink and report the changed module versions via
``runtime/debug.ReadBuildInfo``. Plugin mode is omitted on Windows.
