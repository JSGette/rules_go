// Copyright 2026 The Bazel Authors. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestPrelinkSDKVersions(t *testing.T) {
	for _, tc := range []struct {
		builder string
		input   string
		wantOK  bool
	}{
		{"go1.26.7", "go1.26.7", true},
		{"go1.26.10", "go1.26.10", true},
		{"go1.26.7", "go1.26.10", false},
		{"go1.26.10", "go1.26.7", false},
		{"go1.26.8", "go1.26.8", false},
		{"go1.26.11", "go1.26.11", false},
		{"go1.27.0", "go1.27.0", false},
		{"devel go1.26.10", "devel go1.26.10", false},
		{"go1.26.10", "", false},
		{"go1.26.10", "go1.26.10-custom", false},
	} {
		if err := validatePrelinkSDKVersions(tc.builder, tc.input); (err == nil) != tc.wantOK {
			t.Errorf("builder %q / linker or recipe %q: err = %v, wantOK = %v", tc.builder, tc.input, err, tc.wantOK)
		}
	}
}

func TestReachableNativeObjects(t *testing.T) {
	dir := t.TempDir()
	write := func(name, data string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(data), 0666); err != nil {
			t.Fatal(err)
		}
		return path
	}
	main := write("main.imports", "a\nb\n")
	a := write("a.imports", "blank\nb\n")
	b := write("b.imports", "a\n")
	blank := write("blank.imports", "")
	// None of these sidecars exists. Prelink must select paths without reading
	// their contents; the unused dependency's imports must not be read either.
	selected, used, err := reachableNativeObjects("app", main,
		[]string{"app=wrong-main.imports", "a=" + a, "b=" + b, "blank=" + blank, "unused=missing.imports"},
		[]string{"app=main.native", "a=a.native", "b=old-b.native", "b=test-b.native", "blank=blank.native", "unused=conflicting.native"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"main.native", "a.native", "test-b.native", "blank.native"}; !reflect.DeepEqual(selected, want) {
		t.Fatalf("selected = %v, want %v", selected, want)
	}
	if want := []string{main, a, b, blank}; !reflect.DeepEqual(used, want) {
		t.Fatalf("used = %v, want %v", used, want)
	}
	if _, _, err := reachableNativeObjects("app", "", nil, nil); err == nil {
		t.Fatal("missing main imports accepted")
	}
}

func TestPrelinkPathRelocation(t *testing.T) {
	oldRoot := filepath.Join(t.TempDir(), "producer", "execroot", "main")
	newRoot := filepath.Join(t.TempDir(), "consumer", "execroot", "main")
	oldTree := filepath.Join(oldRoot, "bazel-out", "app.prelink")
	newTree := filepath.Join(newRoot, "bazel-out", "app.prelink")
	newOutput := filepath.Join(newRoot, "bazel-out", "app")
	cases := [][2]string{
		{filepath.Join(oldTree, "go.o"), filepath.Join(newTree, "go.o")},
		{filepath.Join(oldTree, prelinkOutputName), newOutput},
		{"-L" + filepath.Join(oldRoot, "libs"), "-L" + filepath.Join(newRoot, "libs")},
		{"-Wl,-rpath," + filepath.Join(oldRoot, "libs"), "-Wl,-rpath," + filepath.Join(newRoot, "libs")},
		{filepath.Join(filepath.Dir(oldRoot), "external", "cc"), filepath.Join(filepath.Dir(newRoot), "external", "cc")},
		{cgoAbsPlaceholder + "bazel-out/lib", cgoAbsPlaceholder + "bazel-out/lib"},
		{"/usr/bin/gcc", "/usr/bin/gcc"},
	}
	for _, tc := range cases {
		encoded, err := prelinkEncode(tc[0], oldTree, oldRoot)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(encoded, oldRoot) {
			t.Fatalf("producer path retained in %q", encoded)
		}
		decoded, err := prelinkDecode(encoded, newTree, newRoot, newOutput)
		if err != nil {
			t.Fatal(err)
		}
		if decoded != tc[1] {
			t.Errorf("relocated %q = %q, want %q", tc[0], decoded, tc[1])
		}
	}
	if _, err := prelinkEncode(prelinkRootToken, oldTree, oldRoot); err == nil {
		t.Fatal("reserved token accepted")
	}
	if _, err := prelinkDecode(prelinkTokenPrefix+"UNKNOWN__", newTree, newRoot, newOutput); err == nil {
		t.Fatal("unknown token accepted")
	}
}

func TestPrelinkResponse(t *testing.T) {
	tree := t.TempDir()
	response := filepath.Join(tree, "response")
	want := []string{"-o", "file with spaces", `a"b`, "line\nbreak", filepath.Join(tree, "go.o")}
	var lines []string
	for _, arg := range want {
		lines = append(lines, strconv.Quote(arg))
	}
	if err := os.WriteFile(response, []byte(strings.Join(lines, "\n")+"\n"), 0666); err != nil {
		t.Fatal(err)
	}
	got, err := expandPrelinkResponse([]string{"@" + response}, tree)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %q, want %q", got, want)
	}
	if _, err := expandPrelinkResponse([]string{"@" + filepath.Join(tree, "user.rsp")}, tree); err == nil {
		t.Fatal("foreign response accepted")
	}
	if err := os.WriteFile(response, nil, 0666); err != nil {
		t.Fatal(err)
	}
	if got, err := expandPrelinkResponse([]string{"@" + response}, tree); err != nil || len(got) != 0 {
		t.Fatalf("empty response probe: %q, %v", got, err)
	}
	if err := os.WriteFile(response, []byte(strconv.Quote("@nested")+"\n"), 0666); err != nil {
		t.Fatal(err)
	}
	if _, err := expandPrelinkResponse([]string{"@" + response}, tree); err == nil {
		t.Fatal("nested response accepted")
	}
}

func TestPrelinkObjectInsertion(t *testing.T) {
	tree := t.TempDir()
	args := []string{"-m64", "-o", filepath.Join(tree, prelinkOutputName), filepath.Join(tree, "go.o"), filepath.Join(tree, "000000.o"), filepath.Join(tree, "000001.o"), "-lpthread", "native.a"}
	index, err := prelinkObjectInsertion(args, tree)
	if err != nil {
		t.Fatal(err)
	}
	if index != 6 {
		t.Fatalf("insertion index = %d, want after copied objects and before libraries", index)
	}
	if _, err := prelinkObjectInsertion(append(args, filepath.Join(tree, "go.o")), tree); err == nil {
		t.Fatal("duplicate Go object accepted")
	}
	args[2] = filepath.Join(tree, "other-output")
	if _, err := prelinkObjectInsertion(args, tree); err == nil {
		t.Fatal("unexpected output accepted")
	}
}

func TestPrelinkFlagRestrictions(t *testing.T) {
	good := []string{"-m64", "-o", "/tmp/not-linked", "-Wl,--build-id=0x123", "-Wl,--export-dynamic-symbol=crosscall2", "-Wl,--compress-debug-sections=zlib", "-Wl,-z,relro,-z,now", "-Wl,--as-needed", "-Wl,-rpath,$ORIGIN/lib", "-fuse-ld=gold", "-fPIC", "-pthread", "-Lmissing", "-lmissing", "missing.a", "-no-pie"}
	if err := validatePrelinkCCArgs(good); err != nil {
		t.Fatalf("ordinary external flags rejected: %v", err)
	}
	// Default Bazel GCC/gold toolchains use single-dash aliases and retain
	// these flags in cmd/link's capability probes.
	bazelGCC := []string{"-m64", "-s", "-fuse-ld=gold", "-B/usr/bin", "-Wl,-no-as-needed", "-Wl,-z,relro", "-pass-exit-codes", "-Wl,--push-state,-as-needed", "-lstdc++", "-Wl,--pop-state", "-Wl,--push-state,-as-needed", "-lm", "-Wl,--pop-state", "-pthread", "bazel-out/libcdep.a", "-lpthread", "-no-pie"}
	if err := validatePrelinkCCArgs(bazelGCC); err != nil {
		t.Fatalf("default Bazel GCC flags rejected: %v", err)
	}
	for _, arg := range []string{"-Wl,--version-script=missing", "-Wl,-T,missing", "-Wl,-R,missing", "-Wl,--just-symbols=missing", "-Wl,-plugin,missing", "-Wl,@missing", "-Wl,-lmissing", "-Wl,missing.a", "-fplugin=missing", "-fprofile-use=missing", "-fuse-ld=/native/ld", "-fuse-ld=tools/ld", "-fuse-ld=custom", "-isysroot", "-isysroot/native", "--sysroot", "--sysroot=/native", "-m32", "-Xlinker", "-pie", "-shared", "-static", "-oother", "@missing"} {
		if err := validatePrelinkCCArgs([]string{arg}); err == nil {
			t.Errorf("unsupported option accepted: %s", arg)
		}
	}
	for _, linker := range []string{"bfd", "gold", "lld", "mold"} {
		if err := validatePrelinkCCArgs([]string{"-fuse-ld=" + linker}); err != nil {
			t.Errorf("named toolchain linker %s rejected: %v", linker, err)
		}
	}
	base := []string{"-extld", "/usr/bin/cc", "-linkmode", "external", "-extldflags", "-Wl,-z,now", "-buildid=explicit", "-s", "-w"}
	if err := validatePrelinkToolArgs(base); err != nil {
		t.Fatal(err)
	}
	for _, extra := range [][]string{{"-linkmode=internal"}, {"-buildmode", "pie"}, {"-tmpdir=/tmp"}, {"-extld", "/usr/bin/other"}, {"-extld=/usr/bin/other"}, {"-o", "other"}, {"-fipso=other"}, {"-linkshared"}, {"-race"}, {"-H", "windows"}} {
		if err := validatePrelinkToolArgs(append(append([]string{}, base...), extra...)); err == nil {
			t.Errorf("unsupported override accepted: %q", extra)
		}
	}
}

func TestCapturePrelinkReceipt(t *testing.T) {
	tree := t.TempDir()
	// A minimal valid amd64 ELF relocatable header is enough for validating
	// the receipt protocol without invoking a compiler or linker in this test.
	header := make([]byte, 64)
	copy(header, "\x7fELF\x02\x01\x01")
	binary.LittleEndian.PutUint16(header[16:], 1)
	binary.LittleEndian.PutUint16(header[18:], 62)
	binary.LittleEndian.PutUint32(header[20:], 1)
	binary.LittleEndian.PutUint16(header[52:], 64)
	if err := os.WriteFile(filepath.Join(tree, "go.o"), header, 0666); err != nil {
		t.Fatal(err)
	}
	if captured, err := capturePrelinkCC(tree, "/usr/bin/cc", []string{"-o", filepath.Join(tree, "a.out"), filepath.Join(tree, "trivial.c")}); err != nil || captured {
		t.Fatalf("probe was captured: %v, %v", captured, err)
	}
	args := []string{"-m64", "-o", filepath.Join(tree, prelinkOutputName), filepath.Join(tree, "go.o"), "-lpthread"}
	captured, err := capturePrelinkCC(tree, "/usr/bin/cc", args)
	if err != nil || !captured {
		t.Fatalf("capture = %v, %v", captured, err)
	}
	var receipt prelinkReceipt
	if err := readCgoJSON(filepath.Join(tree, prelinkReceiptName), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Compiler != "/usr/bin/cc" || !reflect.DeepEqual(receipt.Args, args) {
		t.Fatalf("receipt changed final command: %+v", receipt)
	}
	if _, err := os.Stat(filepath.Join(tree, prelinkOutputName)); !os.IsNotExist(err) {
		t.Fatalf("capture created an executable: %v", err)
	}
	if _, err := capturePrelinkCC(tree, "/usr/bin/cc", args); err == nil {
		t.Fatal("duplicate final handoff accepted")
	}
}

func TestPrelinkHostResponse(t *testing.T) {
	dir := t.TempDir()
	short := []string{"/usr/bin/cc", "small.o"}
	got, cleanup, err := prelinkHostResponse(short, dir)
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if !reflect.DeepEqual(got, short) {
		t.Fatalf("short command changed: %q", got)
	}
	command := []string{"/usr/bin/cc", "-o", "output with spaces", `a"b.o`, strings.Repeat("x", 31<<10) + ".o", "-lpthread"}
	got, cleanup, err = prelinkHostResponse(command, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if len(got) != 2 || got[0] != command[0] || !strings.HasPrefix(got[1], "@") {
		t.Fatalf("long command not packed: %q", got)
	}
	response := strings.TrimPrefix(got[1], "@")
	data, err := os.ReadFile(response)
	if err != nil {
		t.Fatal(err)
	}
	var decoded []string
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		arg, err := strconv.Unquote(line)
		if err != nil {
			t.Fatal(err)
		}
		decoded = append(decoded, arg)
	}
	if !reflect.DeepEqual(decoded, command[1:]) {
		t.Fatal("response did not preserve arguments")
	}
	cleanup()
	if _, err := os.Stat(response); !os.IsNotExist(err) {
		t.Fatalf("response was not removed: %v", err)
	}
}
