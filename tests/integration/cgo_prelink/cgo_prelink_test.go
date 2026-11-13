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

package cgo_prelink_test

import (
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bazelbuild/rules_go/go/tools/bazel_testing"
)

func TestMain(m *testing.M) {
	bazel_testing.TestMain(m, bazel_testing.Args{Main: `
-- BUILD.bazel --
load("@io_bazel_rules_go//go:def.bzl", "go_binary", "go_library")
load("@rules_cc//cc:cc_library.bzl", "cc_library")

cc_library(
    name = "cdep",
    srcs = ["cdep/native.c"],
    hdrs = ["cdep/native.h"],
)

go_library(
    name = "lib",
    srcs = ["lib/ordinary.go", "lib/bridge.go", "lib/native.c", "lib/secondary.cc", "lib/native.h"],
    cgo = True,
    cdeps = [":cdep"],
    importpath = "example.com/prelink/lib",
)

go_library(
    name = "blank",
    srcs = ["blank/blank.go", "blank/constructor.c"],
    cgo = True,
    importpath = "example.com/prelink/blank",
)

go_library(
    name = "unused",
    srcs = ["unused/unused.go", "unused/conflict.c"],
    cgo = True,
    importpath = "example.com/prelink/unused",
)

go_binary(
    name = "app",
    srcs = ["main.go"],
    # unused is intentionally declared but not imported. Its native definition
    # conflicts with lib, so linking all declared sidecars would fail.
    deps = [":lib", ":blank", ":unused"],
    pure = "off",
)
-- lib/ordinary.go --
package lib

//go:noinline
func Ordinary() int { return 10 }
-- lib/bridge.go --
package lib

/*
#include "native.h"
*/
import "C"

//export GoCallback
func GoCallback(value C.int) C.int { return value + 1 }

func Value() int { return Ordinary() + int(C.native_value()) }
func BlankReady() bool { return C.blank_status() == 7 }
-- lib/native.h --
#ifdef __cplusplus
extern "C" {
#endif
int native_value(void);
int cpp_value(void);
int blank_status(void);
#ifdef __cplusplus
}
#endif
-- lib/native.c --
#include "native.h"
#include "_cgo_export.h"
#include "cdep/native.h"

int blank_ready = 0;
int blank_status(void) { return blank_ready; }
int native_value(void) { return 20 + GoCallback(2) + cdep() + cpp_value(); }
-- lib/secondary.cc --
#include "native.h"
#include <ios>

static int cpp_ready;
struct Startup {
    Startup() { std::ios_base::sync_with_stdio(false); cpp_ready = 7; }
};
static Startup startup;

extern "C" int cpp_value(void) {
    if (cpp_ready != 7) return -1000;
    return 40;
}
-- cdep/native.h --
int cdep(void);
-- cdep/native.c --
#include "native.h"
int cdep(void) { return 30; }
-- blank/blank.go --
package blank
import "C"
-- blank/constructor.c --
extern int blank_ready;
__attribute__((constructor)) static void initialize_blank(void) { blank_ready = 7; }
-- unused/unused.go --
package unused

/* int native_value(void); */
import "C"

func Value() int { return int(C.native_value()) }
-- unused/conflict.c --
int native_value(void) { return -10000; }
-- main.go --
package main

import (
    "crypto/fips140"
    "crypto/sha256"
    "fmt"
    _ "example.com/prelink/blank"
    "example.com/prelink/lib"
)

func main() {
    if !fips140.Enabled() { panic("FIPS mode was not enabled") }
    sum := sha256.Sum256([]byte("abc"))
    if fmt.Sprintf("%x", sum) != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
        panic("SHA256 result changed")
    }
    if !lib.BlankReady() { panic("blank-imported native constructor did not run") }
    fmt.Println(lib.Value())
}
`})
}

type spawn struct {
	Mnemonic      string       `json:"mnemonic"`
	TargetLabel   string       `json:"targetLabel"`
	CacheHit      bool         `json:"cacheHit"`
	Inputs        []spawnFile  `json:"inputs"`
	ActualOutputs []spawnFile  `json:"actualOutputs"`
	Metrics       spawnMetrics `json:"metrics"`
}

type spawnFile struct {
	Path string `json:"path"`
}

type spawnMetrics struct {
	TotalTime         string `json:"totalTime,omitempty"`
	ExecutionWallTime string `json:"executionWallTime,omitempty"`
}

type actionCount struct {
	Executed int `json:"executed"`
	Cached   int `json:"cached"`
}

type observation struct {
	Step         string                  `json:"step"`
	Actions      map[string]actionCount  `json:"actions"`
	Compiles     map[string]actionCount  `json:"compiles"`
	Metrics      map[string]spawnMetrics `json:"metrics"`
	GoSHA256     string                  `json:"goSHA256"`
	BinarySHA256 string                  `json:"binarySHA256"`
	BuildID      string                  `json:"buildID"`
}

type buildSession struct {
	startup, flags  []string
	execRoot        string
	logDir          string
	goObject        string
	executable      string
	nativeArtifacts map[string]bool
}

func requireSupportedHost(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("experimental prelink fixture requires native Linux/amd64")
	}
	if runtime.Version() != "go1.26.7" && runtime.Version() != "go1.26.10" {
		t.Skip("experimental prelink fixture requires stock Go 1.26.7 or 1.26.10")
	}
}

func newBuildSession(t *testing.T, startup, flags []string) *buildSession {
	t.Helper()
	args := append(append([]string{}, startup...), "info", "execution_root")
	execRoot, err := bazel_testing.BazelOutput(args...)
	if err != nil {
		t.Fatal(err)
	}
	return &buildSession{
		startup: startup, flags: flags,
		execRoot: strings.TrimSpace(string(execRoot)), logDir: t.TempDir(),
		nativeArtifacts: make(map[string]bool),
	}
}

func (s *buildSession) build(t *testing.T, step string, want int) observation {
	t.Helper()
	logPath := filepath.Join(s.logDir, step+".json")
	args := append(append([]string{}, s.startup...), "build", "//:app",
		"--features=go_split_cgo", "--features=go_split_link",
		"--remote_download_outputs=all", "--execution_log_json_file="+logPath)
	args = append(args, s.flags...)
	if err := bazel_testing.RunBazel(args...); err != nil {
		t.Fatal(err)
	}
	log, err := os.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	row := observation{
		Step: step, Actions: make(map[string]actionCount),
		Compiles: make(map[string]actionCount), Metrics: make(map[string]spawnMetrics),
	}
	var prelinkInputs []spawnFile
	decoder := json.NewDecoder(log)
	for {
		var action spawn
		if err := decoder.Decode(&action); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("reading execution log: %v", err)
		}
		label := strings.TrimPrefix(action.TargetLabel, "@@")
		if strings.HasPrefix(label, "//:") && (action.Mnemonic == "GoCompilePkg" || action.Mnemonic == "GoCompilePkgExternal") {
			count := row.Compiles[label]
			if action.CacheHit {
				count.Cached++
			} else {
				count.Executed++
			}
			row.Compiles[label] = count
		}
		if action.Mnemonic == "GoCgoCompile" || (label == "//:cdep" && (action.Mnemonic == "CppCompile" || action.Mnemonic == "CppArchive")) {
			for _, output := range action.ActualOutputs {
				s.nativeArtifacts[output.Path] = true
			}
		}
		if label != "//:app" {
			continue
		}
		if action.Mnemonic == "GoPrelink" {
			prelinkInputs = append(prelinkInputs, action.Inputs...)
		}
		count := row.Actions[action.Mnemonic]
		if action.CacheHit {
			count.Cached++
		} else {
			count.Executed++
		}
		row.Actions[action.Mnemonic] = count
		if action.Mnemonic != "GoPrelink" && action.Mnemonic != "GoHostLink" {
			continue
		}
		row.Metrics[action.Mnemonic] = action.Metrics
		for _, output := range action.ActualOutputs {
			if action.Mnemonic == "GoPrelink" && filepath.Base(output.Path) == "go.o" {
				s.goObject = output.Path
			}
			if action.Mnemonic == "GoHostLink" {
				s.executable = output.Path
			}
		}
	}
	// Native actions and prelink may run concurrently, so inspect their inputs
	// only after all outputs in this execution log have been collected.
	for _, input := range prelinkInputs {
		if s.nativeArtifacts[input.Path] || strings.Contains(input.Path, ".cgo_objects") || strings.Contains(input.Path, ".cgo_native") {
			t.Errorf("%s: GoPrelink consumes native artifact %s", step, input.Path)
		}
	}
	if s.goObject == "" || s.executable == "" {
		t.Fatal("build did not report both a GoPrelink go.o and a GoHostLink executable")
	}
	row.GoSHA256 = fileSHA256(t, filepath.Join(s.execRoot, filepath.FromSlash(s.goObject)))
	executable := filepath.Join(s.execRoot, filepath.FromSlash(s.executable))
	row.BinarySHA256 = fileSHA256(t, executable)
	row.BuildID = elfBuildID(t, executable)
	encoded, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("prelink-loop %s", encoded)
	cmd := exec.Command(executable)
	cmd.Env = append(os.Environ(), "GODEBUG=fips140=on")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running app with FIPS enabled: %v\n%s", err, output)
	}
	if got := strings.TrimSpace(string(output)); got != fmt.Sprint(want) {
		t.Fatalf("%s: app printed %q, want %d", step, got, want)
	}
	return row
}

func TestPrelinkEditLoop(t *testing.T) {
	requireSupportedHost(t)
	if err := bazel_testing.RunBazel("clean"); err != nil {
		t.Fatal(err)
	}
	session := newBuildSession(t, nil, nil)
	previous := session.build(t, "cold", 103)
	assertCompileCount(t, previous, "//:lib", 1)
	assertStageCount(t, previous, "GoPrelink", 1)
	assertStageCount(t, previous, "GoHostLink", 1)
	assertNoop(t, previous, session.build(t, "noop", 103))
	for i, edit := range []struct {
		step, path, before, after string
		goEdit                    bool
	}{
		{"c-body", "lib/native.c", "return 20", "return 21", false},
		{"cxx-body", "lib/secondary.cc", "return 40", "return 41", false},
		{"cdep-body", "cdep/native.c", "return 30", "return 31", false},
		{"go-body", "lib/ordinary.go", "return 10", "return 11", true},
	} {
		editSource(t, edit.path, edit.before, edit.after)
		current := session.build(t, edit.step, 104+i)
		wantPrelink := 0
		if edit.goEdit {
			wantPrelink = 1
			assertCompileCount(t, current, "//:lib", 1)
		} else {
			assertCompilesSkipped(t, current)
		}
		assertStageCount(t, current, "GoPrelink", wantPrelink)
		assertStageCount(t, current, "GoHostLink", 1)
		if gotChanged := previous.GoSHA256 != current.GoSHA256; gotChanged != edit.goEdit {
			t.Errorf("%s: go.o changed = %t, want %t", edit.step, gotChanged, edit.goEdit)
		}
		assertBinaryChanged(t, previous, current)
		previous = current
	}
	assertNoop(t, previous, session.build(t, "final-noop", 107))
}

// A native edit in a fresh output base must replay a cached prelink from the
// first execroot. A cache hit for the entire executable would not exercise this.
func TestRelocatedPrelink(t *testing.T) {
	requireSupportedHost(t)
	root := t.TempDir()
	// Register before the shutdown cleanups so servers stop before their
	// read-only output directories are removed, even after a failed build.
	t.Cleanup(func() {
		if err := removeReadOnlyTree(root); err != nil {
			t.Errorf("removing relocated build directories: %v", err)
		}
	})
	flags := []string{"--disk_cache=" + filepath.Join(root, "cache"), "--remote_cache=", "--remote_executor="}
	firstStartup := []string{"--output_base=" + filepath.Join(root, "first")}
	secondStartup := []string{"--output_base=" + filepath.Join(root, "second")}
	for _, startup := range [][]string{firstStartup, secondStartup} {
		t.Cleanup(func() {
			args := append(append([]string{}, startup...), "shutdown")
			if err := bazel_testing.RunBazel(args...); err != nil {
				t.Errorf("shutting down relocated build: %v", err)
			}
		})
	}
	first := newBuildSession(t, firstStartup, flags)
	before := first.build(t, "original-root", 103)
	if err := bazel_testing.RunBazel(append(append([]string{}, firstStartup...), "shutdown")...); err != nil {
		t.Fatal(err)
	}
	// Cached plans must work without access to absolute paths from the first
	// execroot, even when the sandbox would otherwise allow reading them.
	if err := removeReadOnlyTree(filepath.Join(root, "first")); err != nil {
		t.Fatal(err)
	}
	editSource(t, "lib/native.c", "return 20", "return 21")
	second := newBuildSession(t, secondStartup, flags)
	if first.execRoot == second.execRoot {
		t.Fatal("relocation test did not create independent execroots")
	}
	after := second.build(t, "relocated-native-edit", 104)
	if got := after.Compiles["//:lib"]; got.Executed != 0 || got.Cached != 1 {
		t.Errorf("relocated GoCompilePkg = %+v, want exactly one cache hit and no execution", got)
	}
	if got := after.Actions["GoPrelink"]; got.Executed != 0 || got.Cached != 1 {
		t.Errorf("relocated GoPrelink = %+v, want exactly one cache hit and no execution", got)
	}
	if got := after.Actions["GoHostLink"]; got.Executed != 1 || got.Cached != 0 {
		t.Errorf("relocated GoHostLink = %+v, want exactly one execution and no cache hit", got)
	}
	if before.GoSHA256 != after.GoSHA256 {
		t.Error("relocation/native edit changed go.o")
	}
	assertBinaryChanged(t, before, after)
}

// Bazel makes tree-artifact directories read-only. WalkDir does not follow
// symlinks, so this only changes directories inside the test-owned temporary
// tree, never SDK repositories or sources that its symlinks point to.
func removeReadOnlyTree(root string) error {
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		return os.Chmod(path, info.Mode().Perm()|0200)
	})
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return os.RemoveAll(root)
}

func editSource(t *testing.T, path, before, after string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), before) != 1 {
		t.Fatalf("%s: edit must match exactly once", path)
	}
	t.Cleanup(func() {
		if err := os.WriteFile(path, data, 0666); err != nil {
			t.Errorf("restoring %s: %v", path, err)
		}
	})
	if err := os.WriteFile(path, []byte(strings.Replace(string(data), before, after, 1)), 0666); err != nil {
		t.Fatal(err)
	}
}

func assertStageCount(t *testing.T, row observation, mnemonic string, want int) {
	t.Helper()
	count := row.Actions[mnemonic]
	if count.Executed+count.Cached != want {
		t.Errorf("%s: %s executions/cache hits = %+v, want %d total", row.Step, mnemonic, count, want)
	}
}

func assertCompileCount(t *testing.T, row observation, label string, want int) {
	t.Helper()
	count := row.Compiles[label]
	if count.Executed+count.Cached != want {
		t.Errorf("%s: %s GoCompilePkg executions/cache hits = %+v, want %d total", row.Step, label, count, want)
	}
}

func assertCompilesSkipped(t *testing.T, row observation) {
	t.Helper()
	for label, count := range row.Compiles {
		if count.Executed+count.Cached != 0 {
			t.Errorf("%s: %s GoCompilePkg should be reused without execution or a disk/remote cache lookup, got %+v", row.Step, label, count)
		}
	}
}

func assertNoop(t *testing.T, before, after observation) {
	t.Helper()
	assertCompilesSkipped(t, after)
	assertStageCount(t, after, "GoPrelink", 0)
	assertStageCount(t, after, "GoHostLink", 0)
	if before.GoSHA256 != after.GoSHA256 || before.BinarySHA256 != after.BinarySHA256 || before.BuildID != after.BuildID {
		t.Errorf("%s: no-op changed artifacts or build ID", after.Step)
	}
}

func assertBinaryChanged(t *testing.T, before, after observation) {
	t.Helper()
	if before.BinarySHA256 == after.BinarySHA256 {
		t.Errorf("%s: edited program has unchanged executable bytes", after.Step)
	}
	if before.BuildID == after.BuildID {
		t.Errorf("%s: edited program has unchanged content build ID", after.Step)
	}
}

func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func elfBuildID(t *testing.T, path string) string {
	t.Helper()
	f, err := elf.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	section := f.Section(".note.go.buildid")
	if section == nil {
		t.Fatal("executable has no Go build ID section")
	}
	data, err := section.Data()
	if err != nil || len(data) < 12 {
		t.Fatalf("reading Go build ID note: %v (size %d)", err, len(data))
	}
	nameSize := uint64(f.ByteOrder.Uint32(data[:4]))
	descSize := uint64(f.ByteOrder.Uint32(data[4:8]))
	start := uint64(12) + (nameSize+3)&^uint64(3)
	if descSize == 0 || start+descSize > uint64(len(data)) {
		t.Fatal("malformed Go build ID note")
	}
	id := string(data[start : start+descSize])
	if strings.Trim(id, "0/") == "" {
		t.Fatal("Go build ID placeholder was not finalized")
	}
	return id
}
