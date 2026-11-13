#!/usr/bin/env python3
"""Version-pinned Go 1.26.7/Linux amd64 prelink handoff experiment.

Run with Python 3 on Linux/WSL. No Bazel or compiler fork is needed. All
generated files stay in the requested work and cache directories.

The ELF finalization algorithm adapts Go 1.26.7's
src/cmd/link/internal/ld/fips140.go:195-228,403-475.
Copyright 2024 The Go Authors. See LICENSE.go.txt for its BSD license.
This is an experiment pinned to that format, not a public Go linker API.
"""

import argparse
import ast
import hashlib
import hmac
import json
import os
from pathlib import Path
import platform
import re
import shutil
import struct
import subprocess
import sys
import tempfile
import time


SENTINEL = 77


def expanded_arguments(args):
    # Go 1.26.7 writes one %q string per line in its own response file.
    expanded = []
    for arg in args:
        if arg.startswith("@"):
            for line in Path(arg[1:]).read_text().splitlines():
                value = ast.literal_eval(line)
                if not isinstance(value, str):
                    raise ValueError("unexpected Go response-file entry")
                expanded.append(value)
        else:
            expanded.append(arg)
    return expanded


def capture_cc(args):
    argv = expanded_arguments(args)
    go_object = Path(os.environ["GO_PRELINK_OBJECT"]).resolve()
    matches = [arg for arg in argv if not arg.startswith("-") and Path(arg).resolve() == go_object]
    is_final = bool(matches)
    trace = {"final": is_final, "argv": argv}
    with open(os.environ["GO_PRELINK_TRACE"], "a") as stream:
        stream.write(json.dumps(trace) + "\n")
    real_cc = os.environ["GO_PRELINK_REAL_CC"]
    if not is_final:
        os.execv(real_cc, [real_cc, *args])
    if len(matches) != 1 or argv.count("-o") != 1:
        raise RuntimeError("ambiguous final linker invocation")
    outfile = argv[argv.index("-o") + 1]
    if Path(outfile).exists():
        raise RuntimeError("handoff unexpectedly has a linked output")
    data = go_object.read_bytes()
    if data[:6] != b"\x7fELF\x02\x01" or struct.unpack_from("<H", data, 16)[0] != 1:
        raise RuntimeError("handoff go.o is not a Linux ELF64 relocatable object")
    receipt = {
        "format": 1,
        "sdk": "go1.26.7",
        "cwd": os.getcwd(),
        "tool": real_cc,
        "argv": argv,
        "output_index": argv.index("-o") + 1,
        "go_object": str(go_object),
        "go_object_sha256": hashlib.sha256(data).hexdigest(),
        "sentinel_exit": SENTINEL,
    }
    # O_EXCL enforces exactly one handoff. A failed compiler/probe cannot
    # masquerade as a completed prelink action without a valid receipt.
    fd = os.open(os.environ["GO_PRELINK_RECEIPT"], os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o666)
    with os.fdopen(fd, "w") as stream:
        json.dump(receipt, stream, indent=2)
        stream.write("\n")
        stream.flush()
        os.fsync(stream.fileno())
    print("GO_PRELINK_HANDOFF_COMPLETE", file=sys.stderr)
    return SENTINEL


class Elf64:
    def __init__(self, path):
        self.path = Path(path)
        self.data = self.path.read_bytes()
        if self.data[:6] != b"\x7fELF\x02\x01":
            raise ValueError("experiment supports only little-endian ELF64")
        header = struct.unpack_from("<HHIQQQIHHHHHH", self.data, 16)
        self.etype = header[0]
        phoff, shoff = header[4:6]
        phsize, phcount, shsize, shcount, names_index = header[8:13]
        if not shcount or names_index >= shcount:
            raise ValueError("extended ELF section numbering is not supported by this experiment")
        self.programs = [struct.unpack_from("<IIQQQQQQ", self.data, phoff + i * phsize) for i in range(phcount)]
        self.sections = [struct.unpack_from("<IIQQQQIIQQ", self.data, shoff + i * shsize) for i in range(shcount)]
        names_section = self.sections[names_index]
        names = self.data[names_section[4]:names_section[4] + names_section[5]]
        self.named_sections = {}
        for section in self.sections:
            start = section[0]
            end = names.index(b"\x00", start)
            self.named_sections[names[start:end].decode()] = section

    def fips_sum(self):
        section = self.named_sections.get(".go.fipsinfo")
        return None if section is None else self.data[section[4] + 16:section[4] + 48].hex()


def finalize_elf_fips(path):
    """Replay Go 1.26.7's ELF FIPS finalization without private Go APIs."""
    elf = Elf64(path)
    section = elf.named_sections.get(".go.fipsinfo")
    if section is None:
        return {"present": False}
    offset, size = section[4], section[5]
    info = elf.data[offset:offset + size]
    if len(info) != 120 or info[:16] != b"\xff Go fipsinfo \xff\x00":
        raise ValueError("unexpected Go 1.26.7 ELF FIPS metadata")
    before = info[16:48].hex()
    digest = hmac.new(bytes(32), digestmod=hashlib.sha256)
    digest.update(b"go fips object v1\n")
    ranges = []
    for i in range(4):
        start, end = struct.unpack_from("<QQ", info, 56 + 16 * i)
        for prog in elf.programs:
            ptype, _, fileoff, vaddr, _, filesz, _, _ = prog
            if ptype == 1 and vaddr <= start <= end <= vaddr + filesz:
                length = end - start
                begin = fileoff + start - vaddr
                digest.update(struct.pack(">Q", length))
                digest.update(elf.data[begin:begin + length])
                ranges.append({"offset": begin, "length": length})
                break
        else:
            raise ValueError("FIPS range is outside ELF loadable segments")
    checksum = digest.digest()
    with open(path, "r+b") as stream:
        stream.seek(offset + 16)
        stream.write(checksum)
    return {"present": True, "before": before, "after": checksum.hex(), "ranges": ranges}


def command(args, *, cwd, env, check=True):
    result = subprocess.run([str(arg) for arg in args], cwd=cwd, env=env, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    if check and result.returncode:
        raise RuntimeError(f"command failed ({result.returncode}): {args!r}\n{result.stdout}\n{result.stderr}")
    return result


def run_binary(path, *, cwd, env):
    result = command([path], cwd=cwd, env={**env, "GODEBUG": "fips140=on"}, check=False)
    return {"exit": result.returncode, "stdout": result.stdout.strip(), "stderr": result.stderr.strip()}


def replay(receipt, output, *, env, replacement=None):
    argv = list(receipt["argv"])
    argv[receipt["output_index"]] = str(output)
    if replacement:
        old, new = replacement
        if argv.count(old) != 1:
            raise ValueError("native object replacement is ambiguous")
        argv[argv.index(old)] = str(new)
    command([receipt["tool"], *argv], cwd=receipt["cwd"], env=env)


def verify_variant(kind, root, sdk, wrapper, env):
    started = time.monotonic()
    source = root / kind
    source.mkdir()
    (source / "go.mod").write_text("module example.com/go-prelink-probe\n\ngo 1.26\n")
    imports = '"fmt"'
    expression = 'fmt.Printf("%d\\n", int(C.answer()))'
    if kind == "sha256":
        imports += '; "crypto/sha256"'
        expression = 'fmt.Printf("%d %x\\n", int(C.answer()), sha256.Sum256([]byte("prelink")))'
    elif kind == "aes":
        imports += '; "crypto/aes"'
        expression = 'cipher, err := aes.NewCipher(make([]byte, 16)); if err != nil { panic(err) }; dst := make([]byte, 16); cipher.Encrypt(dst, make([]byte, 16)); fmt.Printf("%d %x\\n", int(C.answer()), dst)'
    (source / "main.go").write_text('package main\n/* int answer(void); */\nimport "C"\nimport (' + imports + ')\nfunc main() { ' + expression + ' }\n')
    (source / "native.c").write_text("int answer(void) { return 41; }\n")
    prelink = source / "prelink"
    prelink.mkdir()
    receipt_file, trace_file = source / "receipt.json", source / "invocations.jsonl"
    capture_env = {
        **env,
        "GO_PRELINK_OBJECT": str(prelink / "go.o"),
        "GO_PRELINK_RECEIPT": str(receipt_file),
        "GO_PRELINK_TRACE": str(trace_file),
        "GO_PRELINK_REAL_CC": env["CC"],
    }
    ldflags = f"-linkmode=external -tmpdir={prelink} -extld={wrapper}"
    go = sdk / "bin/go"
    captured_output = source / "must-not-exist"
    capture = command([go, "build", "-trimpath", "-ldflags=" + ldflags, "-o", captured_output, "."], cwd=source, env=capture_env, check=False)
    (source / "capture.stdout").write_text(capture.stdout)
    (source / "capture.stderr").write_text(capture.stderr)
    if not capture.returncode or captured_output.exists() or not receipt_file.is_file():
        raise RuntimeError("expected failed Go link, one receipt, and no final binary")
    if "GO_PRELINK_HANDOFF_COMPLETE" not in capture.stderr or "exit status 77" not in capture.stderr:
        raise RuntimeError("Go link did not stop at the deliberate handoff")
    trace = [json.loads(line) for line in trace_file.read_text().splitlines()]
    if sum(item["final"] for item in trace) != 1:
        raise RuntimeError("expected exactly one final external linker invocation")
    receipt = json.loads(receipt_file.read_text())
    if Elf64(prelink / "go.o").etype != 1:
        raise RuntimeError("missing relocatable Go prelink output")
    raw = source / "replayed-raw"
    replay(receipt, raw, env=env)
    raw_run = run_binary(raw, cwd=source, env=env)
    if kind != "plain" and (raw_run["exit"] == 0 or "fips140: no verification checksum found" not in raw_run["stderr"]):
        raise RuntimeError(f"crypto replay did not demonstrate the missing FIPS finalization: {raw_run}")
    finalized = source / "replayed-finalized"
    shutil.copy2(raw, finalized)
    fips = finalize_elf_fips(finalized)
    command([go, "tool", "buildid", "-w", finalized], cwd=source, env=env)
    final_run = run_binary(finalized, cwd=source, env=env)
    if final_run["exit"] or not final_run["stdout"].startswith("41"):
        raise RuntimeError(f"finalized replay did not return 41: {final_run}")
    final_build_id = command([go, "tool", "buildid", finalized], cwd=source, env=env).stdout.strip()

    native_matches = []
    for arg in receipt["argv"]:
        if re.fullmatch(r"\d{6}\.o", Path(arg).name):
            symbols = command(["nm", "--defined-only", arg], cwd=source, env=env).stdout
            if re.search(r"\b[Tt] answer$", symbols, flags=re.MULTILINE):
                native_matches.append(arg)
    if len(native_matches) != 1:
        raise RuntimeError(f"could not identify exactly one native answer object: {native_matches}")
    (source / "native.c").write_text("int answer(void) { return 42; }\n")
    replacement = source / "native-replacement.o"
    command([env["CC"], "-fPIC", "-O2", "-c", source / "native.c", "-o", replacement], cwd=source, env=env)
    changed = source / "replayed-native-edit"
    replay(receipt, changed, env=env, replacement=(native_matches[0], replacement))
    changed_fips = finalize_elf_fips(changed)
    command([go, "tool", "buildid", "-w", changed], cwd=source, env=env)
    changed_run = run_binary(changed, cwd=source, env=env)
    if changed_run["exit"] or not changed_run["stdout"].startswith("42"):
        raise RuntimeError(f"native-only replay did not return 42: {changed_run}")
    changed_build_id = command([go, "tool", "buildid", changed], cwd=source, env=env).stdout.strip()
    if not final_build_id or not changed_build_id or final_build_id == changed_build_id:
        raise RuntimeError("native-only edit did not change the finalized Go build ID")
    unchanged = hashlib.sha256((prelink / "go.o").read_bytes()).hexdigest() == receipt["go_object_sha256"]
    if not unchanged:
        raise RuntimeError("Go prelink output changed during native-only replay")

    # Compare to a real ordinary stock-Go link of the edited native source.
    baseline_tmp = source / "baseline-prelink"
    baseline_tmp.mkdir()
    baseline = source / "stock-baseline"
    command([go, "build", "-trimpath", f"-ldflags=-linkmode=external -tmpdir={baseline_tmp}", "-o", baseline, "."], cwd=source, env=env)
    baseline_run = run_binary(baseline, cwd=source, env=env)
    if baseline_run != changed_run:
        raise RuntimeError(f"native replay differs from stock executable: {changed_run} != {baseline_run}")
    result = {
        "kind": kind, "capture_exit": capture.returncode,
        "final_invocations": 1, "delegated_probes": len(trace) - 1,
        "captured_output_exists": captured_output.exists(),
        "go_object_sha256": receipt["go_object_sha256"], "go_object_unchanged": unchanged,
        "go_object_has_fips_section": ".go.fipsinfo" in Elf64(prelink / "go.o").named_sections,
        "raw_run_fips_on": raw_run, "finalized_run_fips_on": final_run,
        "native_edit_run_fips_on": changed_run, "stock_baseline_run_fips_on": baseline_run,
        "fips_finalization": fips, "native_edit_fips_finalization": changed_fips,
        "finalized_build_id": final_build_id, "native_edit_build_id": changed_build_id,
        "build_id_changed": final_build_id != changed_build_id,
        "native_object_replaced": native_matches[0],
        "elapsed_seconds": round(time.monotonic() - started, 3),
    }
    (source / "result.json").write_text(json.dumps(result, indent=2) + "\n")
    print(json.dumps(result), flush=True)
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--sdk", type=Path, required=True, help="Unmodified Go 1.26.7 Linux amd64 SDK")
    parser.add_argument("--work-dir", type=Path, help="Parent for a fresh run directory (default: a fresh system temporary directory)")
    parser.add_argument("--cache-dir", type=Path, help="Parent for Go caches and temporary compiler files (default: a fresh system temporary directory)")
    parser.add_argument("--variant", choices=["all", "plain", "sha256", "aes"], default="all")
    args = parser.parse_args()
    sdk = args.sdk.resolve()
    if ((sdk / "VERSION").read_text().splitlines()[0] != "go1.26.7" or sys.platform != "linux"
            or platform.machine().lower() not in ("x86_64", "amd64")):
        raise RuntimeError("experiment requires Go 1.26.7 on a native Linux amd64 host")
    workspace = args.work_dir.resolve() if args.work_dir else Path(tempfile.mkdtemp(prefix="go-prelink-probe-"))
    workspace.mkdir(parents=True, exist_ok=True)
    run_root = Path(tempfile.mkdtemp(prefix="run-", dir=workspace))
    cache = args.cache_dir.resolve() if args.cache_dir else Path(tempfile.mkdtemp(prefix="go-prelink-cache-"))
    for subdir in ["cache", "tmp", "modcache", "gopath"]:
        (cache / subdir).mkdir(parents=True, exist_ok=True)
    real_cc = shutil.which("gcc")
    if not real_cc or not shutil.which("nm"):
        raise RuntimeError("gcc and nm are required")
    env = {**os.environ, "CGO_ENABLED": "1", "GOOS": "linux", "GOARCH": "amd64",
           "CC": real_cc, "GOROOT": str(sdk), "GOTOOLCHAIN": "local", "GOWORK": "off", "GOFLAGS": "", "GOENV": "off",
           "GOFIPS140": "latest", "GOEXPERIMENT": "", "GOAMD64": "v1", "GODEBUG": "",
           "CGO_CFLAGS": "-O2 -g", "CGO_CPPFLAGS": "", "CGO_CXXFLAGS": "-O2 -g", "CGO_LDFLAGS": "-O2 -g",
           "GOCACHE": str(cache / "cache"), "GOMODCACHE": str(cache / "modcache"),
           "GOPATH": str(cache / "gopath"), "TMPDIR": str(cache / "tmp"), "GOTMPDIR": str(cache / "tmp")}
    if command([sdk / "bin/go", "version"], cwd=workspace, env=env).stdout.strip() != "go version go1.26.7 linux/amd64":
        raise RuntimeError("SDK executable does not match the required Go 1.26.7 Linux amd64 version")
    wrapper = run_root / "capture-cc"
    # A separate executable path is needed because linker's capability probes
    # invoke only the first word of -extld.
    wrapper.write_text("#!/usr/bin/env python3\nimport runpy, sys\nsys.argv = [" + repr(str(Path(__file__).resolve())) + ", 'capture', *sys.argv[1:]]\nrunpy.run_path(sys.argv[0], run_name='__main__')\n")
    wrapper.chmod(0o755)
    variants = ["plain", "sha256", "aes"] if args.variant == "all" else [args.variant]
    print("Artifacts: " + str(run_root), flush=True)
    results = [verify_variant(kind, run_root, sdk, wrapper, env) for kind in variants]
    (run_root / "summary.json").write_text(json.dumps(results, indent=2) + "\n")


if __name__ == "__main__":
    if len(sys.argv) > 1 and sys.argv[1] == "capture":
        sys.exit(capture_cc(sys.argv[2:]))
    main()
