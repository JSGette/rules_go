Reproducibility

reproducibility_test
--------------------
Verifies that the files generated when building a set of targets are identical,
even when built from multiple copies of the same workspace.

Currently covers pure ``go_binary`` targets, a cgo ``go_binary`` with
``linkmode = "c-archive"``, and pure and cgo ``go_library`` targets built with
``--collect_code_coverage``.
The covered cgo fixture also verifies that runtime source positions retain the
original nested filename and line number.

TODO: cover more modes. Currently, it seems like a cgo ``go_binary`` that
produces an executable is not reproducible on macOS. This is most likely
due to the external linker, since all the inputs to the linker are identical.
Needs investigation.
