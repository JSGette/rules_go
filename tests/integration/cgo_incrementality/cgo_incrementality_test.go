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

package cgo_incrementality_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/bazelbuild/rules_go/go/tools/bazel_testing"
)

func TestMain(m *testing.M) {
	bazel_testing.TestMain(m, bazel_testing.Args{Main: `
-- BUILD.bazel --
load("@io_bazel_rules_go//go:def.bzl", "go_binary", "go_library", "go_test")
load("@rules_cc//cc:cc_library.bzl", "cc_library")

cc_library(
    name = "cdep",
    srcs = ["cdep/native.c"],
    hdrs = ["cdep/native.h"],
)

go_library(
    name = "go_dep",
    srcs = ["go_dep/dep.go"],
    importpath = "example.com/incremental/go_dep",
)

go_library(
    name = "lib",
    srcs = ["ordinary.go", "nested/bridge.go", "embedded.go", "native.c", "secondary.cc", "native.h"],
    embedsrcs = ["embedded/data.txt"],
    cdeps = [":cdep"],
    cgo = True,
    cppopts = ["-DNATIVE_OPTION=0"],
    copts = ["-DC_ONLY=0"],
    cxxopts = ["-DCXX_ONLY=0"],
    deps = [":go_dep"],
    importpath = "example.com/incremental/lib",
)

go_library(
    name = "consumer",
    srcs = ["consumer/consumer.go"],
    deps = [":lib"],
    importpath = "example.com/incremental/consumer",
)

go_binary(
    name = "app",
    srcs = ["main/main.go"],
    deps = [":consumer", ":lib"],
    pure = "off",
)

go_test(
    name = "native_only_test",
    srcs = ["native_only/ordinary_test.go", "native_only/unused.c"],
    cgo = True,
)

go_library(
    name = "native_asm",
    srcs = ["native_asm/bridge.go", "native_asm/value_amd64.S"],
    cgo = True,
    importpath = "example.com/incremental/native_asm",
)

go_binary(
    name = "native_asm_app",
    srcs = ["native_asm/main/main.go"],
    deps = [":native_asm"],
    pure = "off",
)

# Match tests/core/go_library's .syso fixture: cc_library selects the native
# toolchain, and the genrule only changes the archive's extension.
cc_library(
    name = "retained_native",
    srcs = ["retained/native.c"],
    linkstatic = True,
)

genrule(
    name = "retained_syso",
    srcs = [":retained_native"],
    outs = ["retained/imported.syso"],
    cmd = "cp $< $@",
)

go_library(
    name = "retained",
    srcs = [
        "retained/retained.go",
        "retained/retained_amd64.s",
        "retained/retained_arm64.s",
        "asm_headers/value.h",
        "precedence.h",
        "retained/precedence.h",
        ":retained_syso",
    ],
    cgo = True,
    importpath = "example.com/incremental/retained",
)

go_test(
    name = "retained_test",
    srcs = ["retained/retained_test.go"],
    deps = [":retained"],
)
-- ordinary.go --
package lib

import (
    "example.com/incremental/go_dep"
    "runtime"
)

//go:noinline
func Ordinary() int { return 10 + go_dep.Value() }

//go:noinline
func OrdinaryPosition() (string, int) { _, file, line, _ := runtime.Caller(0); return file, line }
-- go_dep/dep.go --
package go_dep

//go:noinline
func Value() int { return 0 }
-- nested/bridge.go --
package lib

/*
#include "native.h"
*/
import "C"

import "runtime"

//go:noinline
func Native() int { return int(C.native()) + 20 }

//go:noinline
func CgoPosition() (string, int) { _, file, line, _ := runtime.Caller(0); return file, line }

//go:noinline
//line templates/origin.tpl:100
func TemplatePosition() (string, int) { _, file, line, _ := runtime.Caller(0); return file, line }
-- embedded.go --
package lib

import _ "embed"

//go:embed embedded/data.txt
var embedded string

//go:noinline
func Embedded() string { return embedded }
-- embedded/data.txt --
retained embedded data
-- native.c --
#include "native.h"
#include "cdep/native.h"
int native(void) { return 30 + HEADER_VALUE + NATIVE_OPTION + C_ONLY + cdep() + secondary(); }
-- secondary.cc --
#include "native.h"
#include <ios>
extern "C" int secondary(void) { std::ios_base::sync_with_stdio(false); return 0 + CXX_ONLY; }
-- native.h --
#define HEADER_VALUE 40
#ifdef __cplusplus
extern "C" {
#endif
int native(void);
int secondary(void);
#ifdef __cplusplus
}
#endif
-- cdep/native.h --
int cdep(void);
-- cdep/native.c --
#include "native.h"
int cdep(void) { return 0; }
-- consumer/consumer.go --
package consumer

import "example.com/incremental/lib"

//go:noinline
func Value() int { return lib.Ordinary() + lib.Native() }
-- main/main.go --
package main

import (
    "fmt"
    "path/filepath"
    "example.com/incremental/consumer"
    "example.com/incremental/lib"
)

func main() {
    if lib.Embedded() != "retained embedded data\n" { panic("ordinary Go embedded data was not retained") }
    file, line := lib.OrdinaryPosition()
    checkPosition(file, line, "ordinary.go", 12)
    file, line = lib.CgoPosition()
    checkPosition(file, line, "nested/bridge.go", 14)
    file, line = lib.TemplatePosition()
    checkPosition(file, line, "templates/origin.tpl", 100)
    fmt.Println(consumer.Value())
}

func checkPosition(file string, line int, wantFile string, wantLine int) {
    if filepath.ToSlash(file) != wantFile || line != wantLine {
        panic(fmt.Sprintf("source location = %s:%d, want %s:%d", file, line, wantFile, wantLine))
    }
}
-- native_only/ordinary_test.go --
package native_only

import "testing"

func value() int { return 42 }
func TestOrdinaryGoIsRetained(t *testing.T) {
    if value() != 42 { t.Fatal("ordinary Go function was not retained") }
}
-- native_only/unused.c --
int unused(void) { return 42; }
-- native_asm/bridge.go --
package native_asm

// int native_asm_value(void);
import "C"

//go:noinline
func Value() int { return int(C.native_asm_value()) }
-- native_asm/value_amd64.S --
.text
.globl native_asm_value
.type native_asm_value,@function
native_asm_value:
    mov $42, %eax
    ret
.section .note.GNU-stack,"",@progbits
-- native_asm/main/main.go --
package main

import (
    "fmt"
    "example.com/incremental/native_asm"
)

func main() { fmt.Println(native_asm.Value()) }
-- retained/native.c --
int retained_native(void) { return 2; }
-- asm_headers/value.h --
#define ASSEMBLY_VALUE 40
-- precedence.h --
#define SELECTED_VALUE ASSEMBLY_VALUE
-- retained/precedence.h --
#define SELECTED_VALUE 4000
-- retained/retained.go --
package retained

import "runtime"

func assemblyValue() int32
func assemblyPC() uintptr
func nativeValue() int32
func Value() int32 { return assemblyValue() + nativeValue() }
func AssemblyPosition() (string, int) {
    pc := assemblyPC()
    return runtime.FuncForPC(pc).FileLine(pc)
}
-- retained/retained_amd64.s --
#include "textflag.h"
#include "../asm_headers/value.h"
#include "precedence.h"

TEXT ·assemblyValue(SB), NOSPLIT, $0-4
    MOVL $SELECTED_VALUE, ret+0(FP)
    RET

TEXT ·assemblyPC(SB), NOSPLIT, $0-8
    LEAQ ·assemblyValue(SB), AX
    MOVQ AX, ret+0(FP)
    RET

TEXT ·nativeValue(SB), NOSPLIT, $0-4
    CALL retained_native(SB)
    MOVL AX, ret+0(FP)
    RET
-- retained/retained_arm64.s --
#include "textflag.h"
#include "../asm_headers/value.h"
#include "precedence.h"

TEXT ·assemblyValue(SB), NOSPLIT, $0-4
    MOVW $SELECTED_VALUE, R0
    MOVW R0, ret+0(FP)
    RET

TEXT ·assemblyPC(SB), NOSPLIT, $0-8
    MOVD $·assemblyValue(SB), R0
    MOVD R0, ret+0(FP)
    RET

TEXT ·nativeValue(SB), NOSPLIT, $0-4
    BL retained_native(SB)
    MOVW R0, ret+0(FP)
    RET
-- retained/retained_test.go --
package retained_test

import (
    "path/filepath"
    "runtime"
    "testing"
    "example.com/incremental/retained"
)

func TestRetainedObjects(t *testing.T) {
    if got := retained.Value(); got != 42 {
        t.Fatalf("Go assembly plus .syso result = %d, want 42", got)
    }
    file, line := retained.AssemblyPosition()
    wantFile := "retained/retained_" + runtime.GOARCH + ".s"
    if filepath.ToSlash(file) != wantFile || line != 6 {
        t.Fatalf("assembly source location = %s:%d, want %s:6", file, line, wantFile)
    }
}
`})
}

// These are the fields needed from Bazel's SpawnExec proto. The JSON execution
// log is a stream of concatenated messages, not a JSON array or one-line JSON.
// See https://github.com/bazelbuild/bazel/blob/9.3.1/src/main/protobuf/spawn.proto.
type spawn struct {
	Mnemonic      string       `json:"mnemonic"`
	TargetLabel   string       `json:"targetLabel"`
	Runner        string       `json:"runner"`
	CacheHit      bool         `json:"cacheHit"`
	Inputs        []spawnFile  `json:"inputs"`
	ActualOutputs []spawnFile  `json:"actualOutputs"`
	Metrics       spawnMetrics `json:"metrics"`
}

type spawnFile struct {
	Path string `json:"path"`
}

// Preserve protobuf duration strings verbatim. An omitted metric is unknown,
// not a measured zero; these describe the entire GoLink spawn, not its children.
type spawnMetrics struct {
	TotalTime         string `json:"totalTime,omitempty"`
	ExecutionWallTime string `json:"executionWallTime,omitempty"`
}

type linkObservation struct {
	Target   string       `json:"target"`
	Runner   string       `json:"runner"`
	CacheHit bool         `json:"cacheHit"`
	Metrics  spawnMetrics `json:"metrics"`
}

type nativeCompile struct {
	Source   string `json:"source"`
	Object   string `json:"object"`
	CacheHit bool   `json:"cacheHit"`
}

type actionCount struct {
	Executed int `json:"executed"`
	Cached   int `json:"cached"`
}

type observation struct {
	Variant        string                 `json:"variant"`
	Step           string                 `json:"step"`
	ElapsedMillis  int64                  `json:"elapsedMillis"`
	Actions        map[string]actionCount `json:"actions"`
	SHA256         map[string]string      `json:"sha256"`
	NativeCompiles []nativeCompile        `json:"nativeCompiles,omitempty"`
	Links          []linkObservation      `json:"links,omitempty"`
}

func TestEditLoop(t *testing.T) {
	for _, variant := range []string{"baseline", "split"} {
		t.Run(variant, func(t *testing.T) {
			runEditLoop(t, variant)
		})
	}
}

// rules_go also permits native sources without any Go file importing C.
// Preparation produces only objects in this case, so the Go stage must retain
// the ordinary Go files instead of replacing them with an empty generated list.
func TestNativeOnly(t *testing.T) {
	if err := bazel_testing.RunBazel("test", "//:native_only_test", "--features=go_split_cgo"); err != nil {
		t.Fatal(err)
	}
}

func TestNativeAssemblyEdit(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("native assembly fixture uses Linux amd64 syntax")
	}
	if err := bazel_testing.RunBazel("clean"); err != nil {
		t.Fatal(err)
	}
	execRootBytes, err := bazel_testing.BazelOutput("info", "execution_root")
	if err != nil {
		t.Fatal(err)
	}
	execRoot := strings.TrimSpace(string(execRootBytes))
	logDir := t.TempDir()
	artifacts := make(map[string]bool)
	var executable, goArchive, fullArchive string
	build := func(step string, want int) observation {
		t.Helper()
		logPath := filepath.Join(logDir, step+".json")
		if err := bazel_testing.RunBazel("build", "//:native_asm_app",
			"--features=go_split_cgo", "--features=-go_split_link",
			"--remote_download_outputs=all", "--execution_log_json_file="+logPath); err != nil {
			t.Fatal(err)
		}
		log, err := os.Open(logPath)
		if err != nil {
			t.Fatal(err)
		}
		spawns, err := readSpawns(log)
		log.Close()
		if err != nil {
			t.Fatal(err)
		}
		row := observation{Variant: "split-native-assembly", Step: step, Actions: make(map[string]actionCount), SHA256: make(map[string]string)}
		for _, action := range spawns {
			label := strings.TrimPrefix(action.TargetLabel, "@@")
			if label != "//:native_asm" && label != "//:native_asm_app" {
				continue
			}
			key := label + " " + action.Mnemonic
			count := row.Actions[key]
			if action.CacheHit {
				count.Cached++
			} else {
				count.Executed++
			}
			row.Actions[key] = count
			for _, output := range action.ActualOutputs {
				if strings.HasSuffix(output.Path, ".a") || strings.HasSuffix(output.Path, ".x") || strings.HasSuffix(output.Path, ".o") {
					artifacts[output.Path] = true
				}
				if label == "//:native_asm" && strings.HasSuffix(output.Path, ".go.a") {
					goArchive = output.Path
				}
				if label == "//:native_asm" && strings.HasSuffix(output.Path, ".a") && !strings.HasSuffix(output.Path, ".go.a") {
					fullArchive = output.Path
				}
				if action.Mnemonic == "GoLink" && !strings.HasSuffix(output.Path, ".unused_buildinfo_inputs") {
					executable = output.Path
					artifacts[output.Path] = true
				}
			}
		}
		if executable == "" || goArchive == "" || fullArchive == "" {
			t.Fatal("native assembly build did not report executable and both package archives")
		}
		for path := range artifacts {
			data, err := os.ReadFile(filepath.Join(execRoot, filepath.FromSlash(path)))
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(data)
			row.SHA256[path] = hex.EncodeToString(digest[:])
		}
		encoded, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("native-assembly-loop %s", encoded)
		output, err := exec.Command(filepath.Join(execRoot, filepath.FromSlash(executable))).CombinedOutput()
		if err != nil || strings.TrimSpace(string(output)) != fmt.Sprint(want) {
			t.Fatalf("%s: native assembly app = %q, %v; want %d", step, output, err, want)
		}
		return row
	}
	before := build("cold", 42)
	const source = "native_asm/value_amd64.S"
	original, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.WriteFile(source, original, 0666); err != nil {
			t.Errorf("restoring native assembly: %v", err)
		}
	})
	if strings.Count(string(original), "mov $42") != 1 {
		t.Fatal("native assembly edit must match exactly once")
	}
	if err := os.WriteFile(source, []byte(strings.Replace(string(original), "mov $42", "mov $43", 1)), 0666); err != nil {
		t.Fatal(err)
	}
	after := build("native-assembly-body", 43)
	assertGoCompilesSkipped(t, after)
	for _, mnemonic := range []string{"GoCgoCompile", "GoPack"} {
		if count := after.Actions["//:native_asm "+mnemonic]; count.Executed+count.Cached != 1 {
			t.Errorf("native assembly edit: %s = %+v, want one execution or disk/remote-cache hit", mnemonic, count)
		}
	}
	if before.SHA256[goArchive] != after.SHA256[goArchive] {
		t.Error("native assembly body edit changed the Go-only archive")
	}
	if before.SHA256[fullArchive] == after.SHA256[fullArchive] || before.SHA256[executable] == after.SHA256[executable] {
		t.Error("native assembly body edit did not change compatibility archive and executable")
	}
	assertNoop(t, after, build("noop", 43))
}

func TestGoAssemblyAndSysoRetained(t *testing.T) {
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skip("fixture has Go assembly implementations for amd64 and arm64")
	}
	if err := bazel_testing.RunBazel("test", "//:retained_test", "--features=go_split_cgo", "--remote_download_outputs=all"); err != nil {
		t.Fatal(err)
	}
	bin, err := bazel_testing.BazelOutput("info", "bazel-bin", "--features=go_split_cgo")
	if err != nil {
		t.Fatal(err)
	}
	binDir := strings.TrimSpace(string(bin))
	full, err := os.ReadFile(filepath.Join(binDir, "retained.a"))
	if err != nil {
		t.Fatal(err)
	}
	goOnly, err := os.ReadFile(filepath.Join(binDir, "retained_", "retained.go.a"))
	if err != nil {
		t.Fatal(err)
	}
	// No objects came from cgo here: Go assembly and .syso must remain in
	// both archives, with the same member order and contents.
	if !bytes.Equal(full, goOnly) {
		t.Fatal("Go-only archive differs from full archive containing only Go, Go assembly, and .syso")
	}
}

func runEditLoop(t *testing.T, variant string) {
	// bazel_testing deliberately shares an output base across integration tests.
	// Clean it so the baseline discovers every fixture output even on reruns.
	if err := bazel_testing.RunBazel("clean"); err != nil {
		t.Fatal(err)
	}
	execRootBytes, err := bazel_testing.BazelOutput("info", "execution_root")
	if err != nil {
		t.Fatal(err)
	}
	execRoot := strings.TrimSpace(string(execRootBytes))
	logDir := t.TempDir()
	artifacts := make(map[string]string) // execroot-relative path -> target label
	preparedArtifacts := make(map[string]bool)
	generatedArtifacts := make(map[string]bool)
	nativeObjects := make(map[string]string) // original source -> declared object
	nativeArtifacts := make(map[string]bool)
	var executable string
	var goOnlyArchive string
	var fullArchive string
	var exportArchive string

	build := func(step string, want int) observation {
		t.Helper()
		logPath := filepath.Join(logDir, step+".json")
		start := time.Now()
		args := []string{"build", "//:app",
			"--remote_download_outputs=all", "--execution_log_json_file=" + logPath}
		if variant == "split" {
			args = append(args, "--features=go_split_cgo")
		} else {
			args = append(args, "--features=-go_split_cgo")
		}
		err := bazel_testing.RunBazel(args...)
		elapsed := time.Since(start)
		if err != nil {
			t.Fatal(err)
		}
		log, err := os.Open(logPath)
		if err != nil {
			t.Fatal(err)
		}
		spawns, err := readSpawns(log)
		log.Close()
		if err != nil {
			t.Fatal(err)
		}
		row := observation{
			Variant: variant, Step: step, ElapsedMillis: elapsed.Milliseconds(),
			Actions: make(map[string]actionCount), SHA256: make(map[string]string),
		}
		var compileActions, packActions []spawn
		for _, s := range spawns {
			label := strings.TrimPrefix(s.TargetLabel, "@@")
			if label != "//:lib" && label != "//:consumer" && label != "//:app" && label != "//:go_dep" && label != "//:cdep" {
				continue
			}
			key := label + " " + s.Mnemonic
			count := row.Actions[key]
			if s.CacheHit {
				count.Cached++
			} else {
				count.Executed++
			}
			row.Actions[key] = count
			if label == "//:lib" {
				switch s.Mnemonic {
				case "GoCompilePkg":
					compileActions = append(compileActions, s)
				case "GoPack":
					packActions = append(packActions, s)
				}
			}
			if label == "//:lib" && s.Mnemonic == "GoCgoCompile" {
				compile := nativeCompile{CacheHit: s.CacheHit}
				for _, input := range s.Inputs {
					if input.Path == "native.c" || input.Path == "secondary.cc" {
						if compile.Source != "" {
							t.Fatalf("GoCgoCompile consumes both native source files: %+v", s.Inputs)
						}
						compile.Source = input.Path
					}
				}
				for _, output := range s.ActualOutputs {
					if strings.HasSuffix(output.Path, ".o") {
						if compile.Object != "" {
							t.Fatalf("GoCgoCompile produced multiple objects: %+v", s.ActualOutputs)
						}
						compile.Object = output.Path
					}
				}
				if compile.Source == "" || compile.Object == "" {
					t.Fatalf("cannot identify native compile: %+v", compile)
				}
				if old := nativeObjects[compile.Source]; old != "" && old != compile.Object {
					t.Fatalf("object path for %s changed from %s to %s", compile.Source, old, compile.Object)
				}
				nativeObjects[compile.Source] = compile.Object
				row.NativeCompiles = append(row.NativeCompiles, compile)
			}
			if s.Mnemonic == "GoLink" {
				row.Links = append(row.Links, linkObservation{label, s.Runner, s.CacheHit, s.Metrics})
			}
			for _, output := range s.ActualOutputs {
				if s.Mnemonic == "GoCgoCompile" || (label == "//:cdep" && (s.Mnemonic == "CppCompile" || s.Mnemonic == "CppArchive")) {
					nativeArtifacts[output.Path] = true
				}
				if label == "//:lib" && strings.HasSuffix(output.Path, ".go.a") {
					goOnlyArchive = output.Path
				}
				if label == "//:lib" && strings.HasSuffix(output.Path, ".a") && !strings.HasSuffix(output.Path, ".go.a") {
					fullArchive = output.Path
				}
				if label == "//:lib" && strings.HasSuffix(output.Path, ".x") {
					exportArchive = output.Path
				}
				if s.Mnemonic == "GoCgoPrepare" {
					preparedArtifacts[output.Path] = true
				}
				if s.Mnemonic == "GoCgoGenerate" {
					generatedArtifacts[output.Path] = true
				}
				if s.Mnemonic == "GoCgoPrepare" || s.Mnemonic == "GoCgoGenerate" || s.Mnemonic == "GoCgoCompile" || strings.HasSuffix(output.Path, ".a") || strings.HasSuffix(output.Path, ".x") {
					artifacts[output.Path] = label
				}
				if label == "//:app" && s.Mnemonic == "GoLink" && !strings.HasSuffix(output.Path, ".unused_buildinfo_inputs") {
					executable = output.Path
					artifacts[output.Path] = label
				}
			}
		}
		if variant == "split" {
			// Spawns may be logged in any order. Check the data dependencies
			// after discovering every native output, not their execution order.
			for _, action := range compileActions {
				assertCompileBoundary(t, step, action, nativeArtifacts, goOnlyArchive, fullArchive, exportArchive)
			}
			for _, action := range packActions {
				assertPackBoundary(t, step, action, goOnlyArchive, fullArchive)
			}
		}
		for path := range artifacts {
			data, err := os.ReadFile(filepath.Join(execRoot, filepath.FromSlash(path)))
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(data)
			row.SHA256[path] = hex.EncodeToString(digest[:])
		}
		encoded, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		// JSON rows are retained in Bazel's test.log even for a successful test.
		// Elapsed time is diagnostic only: execution logging itself adds overhead.
		t.Logf("edit-loop %s", encoded)
		if executable == "" {
			t.Fatal("baseline did not log the app's GoLink output")
		}
		output, err := exec.Command(filepath.Join(execRoot, filepath.FromSlash(executable))).CombinedOutput()
		if err != nil {
			t.Fatalf("running app: %v\n%s", err, output)
		}
		if got := strings.TrimSpace(string(output)); got != fmt.Sprint(want) {
			t.Fatalf("%s: app printed %q, want %d", step, got, want)
		}
		return row
	}

	baseline := build("cold", 100)
	if variant == "split" {
		assertActionRefreshed(t, baseline, "GoCgoProject")
		assertActionRefreshed(t, baseline, "GoCgoGenerate")
		assertActionRefreshed(t, baseline, "GoCgoPrepare")
		assertActionRefreshed(t, baseline, "GoCompilePkg")
		assertActionRefreshed(t, baseline, "GoPack")
		assertNativeCompiles(t, baseline, []string{"native.c", "secondary.cc"})
		if len(preparedArtifacts) == 0 {
			t.Fatal("baseline did not log prepared cgo outputs")
		}
		if len(generatedArtifacts) == 0 {
			t.Fatal("baseline did not log generated cgo outputs")
		}
		if goOnlyArchive == "" {
			t.Fatal("baseline did not log a Go-only archive")
		}
	} else if goOnlyArchive != "" {
		t.Fatal("combined cgo action unexpectedly produced a Go-only archive")
	}
	for _, label := range []string{"//:lib", "//:consumer", "//:app", "//:go_dep"} {
		for _, ext := range []string{".a", ".x"} {
			found := false
			for path, owner := range artifacts {
				found = found || owner == label && strings.HasSuffix(path, ext)
			}
			if !found {
				t.Fatalf("baseline did not log %s's %s output", label, ext)
			}
		}
	}
	assertNoop(t, baseline, build("noop", 100))

	previous := baseline
	edits := []struct {
		step, path, before, after, changedArchive string
		preservePrepare, preparedOutputChanges    bool
	}{
		{"ordinary-go-body", "ordinary.go", "return 10", "return 11", "//:lib", true, false},
		{"cgo-go-body", "nested/bridge.go", "+ 20", "+ 21", "//:lib", false, true},
		{"c-body", "native.c", "return 30", "return 31", "//:lib", false, true},
		{"cxx-body", "secondary.cc", "return 0", "return 1", "//:lib", false, true},
		{"header", "native.h", "HEADER_VALUE 40", "HEADER_VALUE 41", "//:lib", false, true},
		{"go-dependency-body", "go_dep/dep.go", "return 0", "return 1", "//:go_dep", true, false},
		// cdeps are linked separately: a body edit refreshes preparation but
		// need not change its objects or the containing Go package's archive.
		{"cdep-body", "cdep/native.c", "return 0", "return 1", "//:cdep", false, false},
		{"cppopts", "BUILD.bazel", "-DNATIVE_OPTION=0", "-DNATIVE_OPTION=1", "//:lib", false, true},
		{"copts", "BUILD.bazel", "-DC_ONLY=0", "-DC_ONLY=1", "//:lib", false, true},
		{"cxxopts", "BUILD.bazel", "-DCXX_ONLY=0", "-DCXX_ONLY=1", "//:lib", false, true},
	}
	for i, edit := range edits {
		data, err := os.ReadFile(edit.path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(string(data), edit.before) != 1 {
			t.Fatalf("%s: edit must match exactly once", edit.path)
		}
		path := edit.path
		t.Cleanup(func() {
			if err := os.WriteFile(path, data, 0666); err != nil {
				t.Errorf("restoring %s: %v", path, err)
			}
		})
		if err := os.WriteFile(edit.path, []byte(strings.Replace(string(data), edit.before, edit.after, 1)), 0666); err != nil {
			t.Fatal(err)
		}
		current := build(edit.step, 101+i)
		if variant == "split" {
			// Native body edits must stop before the Go compiler, while the
			// conventional archive still picks up changed native members.
			goArchiveChanged := previous.SHA256[goOnlyArchive] != current.SHA256[goOnlyArchive]
			switch edit.step {
			case "c-body", "cxx-body", "cdep-body":
				assertGoCompilesSkipped(t, current)
				if goArchiveChanged {
					t.Errorf("%s: native body edit changed the Go-only archive", edit.step)
				}
				if previous.SHA256[exportArchive] != current.SHA256[exportArchive] {
					t.Errorf("%s: native body edit changed the export archive", edit.step)
				}
				if edit.step == "cdep-body" {
					assertActionSkipped(t, current, "GoPack")
					if previous.SHA256[fullArchive] != current.SHA256[fullArchive] {
						t.Errorf("%s: separately linked native dependency changed the compatibility archive", edit.step)
					}
				} else {
					assertActionRefreshed(t, current, "GoPack")
					if previous.SHA256[fullArchive] == current.SHA256[fullArchive] {
						t.Errorf("%s: packed native body edit did not change the compatibility archive", edit.step)
					}
				}
			case "ordinary-go-body", "cgo-go-body":
				assertActionRefreshed(t, current, "GoCompilePkg")
				assertActionRefreshed(t, current, "GoPack")
				if !goArchiveChanged {
					t.Errorf("%s: Go body edit did not change the Go-only archive", edit.step)
				}
			}
			var wantNative []string
			switch edit.step {
			case "header", "cppopts":
				assertActionRefreshed(t, current, "GoCgoGenerate")
				wantNative = []string{"native.c", "secondary.cc"}
			case "copts":
				assertActionRefreshed(t, current, "GoCgoGenerate")
				wantNative = []string{"native.c"}
			case "cgo-go-body":
				assertActionRefreshed(t, current, "GoCgoGenerate")
			default:
				assertActionSkipped(t, current, "GoCgoGenerate")
				for path := range generatedArtifacts {
					if previous.SHA256[path] != current.SHA256[path] {
						t.Errorf("%s: generated cgo output changed: %s", edit.step, path)
					}
				}
				if edit.step == "c-body" || edit.step == "cxx-body" {
					wantNative = []string{edit.path}
				} else if edit.step == "cxxopts" {
					wantNative = []string{"secondary.cc"}
				}
			}
			assertNativeCompiles(t, current, wantNative)
			for source, object := range nativeObjects {
				compiled := false
				for _, wanted := range wantNative {
					compiled = compiled || source == wanted
				}
				if !compiled && previous.SHA256[object] != current.SHA256[object] {
					t.Errorf("%s: unchanged native source %s has different object bytes", edit.step, source)
				}
			}
			preparedChanged := false
			for path := range preparedArtifacts {
				preparedChanged = preparedChanged || previous.SHA256[path] != current.SHA256[path]
			}
			if edit.preservePrepare {
				count := current.Actions["//:lib GoCgoPrepare"]
				if count.Executed+count.Cached != 0 {
					t.Errorf("%s: GoCgoPrepare should be reused without a spawn or disk/remote cache lookup, got %+v", edit.step, count)
				}
				if preparedChanged {
					t.Errorf("%s: prepared cgo output digests changed", edit.step)
				}
			} else {
				assertActionRefreshed(t, current, "GoCgoPrepare")
				if edit.preparedOutputChanges && !preparedChanged {
					t.Errorf("%s: prepared cgo output digests did not change", edit.step)
				}
			}
		}
		changed := false
		for path, owner := range artifacts {
			if owner == edit.changedArchive && strings.HasSuffix(path, ".a") && previous.SHA256[path] != current.SHA256[path] {
				changed = true
			}
		}
		if !changed {
			t.Fatalf("%s: %s archive did not change", edit.step, edit.changedArchive)
		}
		previous = current
	}
	assertNoop(t, previous, build("final-noop", 100+len(edits)))
}

func assertCompileBoundary(t *testing.T, step string, action spawn, nativeArtifacts map[string]bool, goArchive, fullArchive, exportArchive string) {
	t.Helper()
	for _, input := range action.Inputs {
		if input.Path == "native.c" || input.Path == "secondary.cc" || input.Path == "cdep/native.c" ||
			nativeArtifacts[input.Path] || strings.Contains(input.Path, ".cgo_objects") || strings.Contains(input.Path, ".cgo_native") {
			t.Errorf("%s: GoCompilePkg consumes native source or object %s", step, input.Path)
		}
	}
	var hasGo, hasExport bool
	for _, output := range action.ActualOutputs {
		hasGo = hasGo || output.Path == goArchive
		hasExport = hasExport || output.Path == exportArchive
		if output.Path == fullArchive {
			t.Errorf("%s: GoCompilePkg still produces the compatibility archive", step)
		}
	}
	if !hasGo || !hasExport {
		t.Errorf("%s: GoCompilePkg must produce the Go-only and export archives", step)
	}
}

func assertPackBoundary(t *testing.T, step string, action spawn, goArchive, fullArchive string) {
	t.Helper()
	var hasGo, hasNative, hasFull bool
	for _, input := range action.Inputs {
		hasGo = hasGo || input.Path == goArchive
		hasNative = hasNative || strings.Contains(input.Path, ".cgo_objects") || strings.Contains(input.Path, ".cgo_native")
	}
	for _, output := range action.ActualOutputs {
		hasFull = hasFull || output.Path == fullArchive
		if output.Path == goArchive || strings.HasSuffix(output.Path, ".x") {
			t.Errorf("%s: GoPack produces a compiler output %s", step, output.Path)
		}
	}
	if !hasGo || !hasNative || !hasFull {
		t.Errorf("%s: GoPack must consume the Go-only archive and native objects and produce the compatibility archive", step)
	}
}

func assertGoCompilesSkipped(t *testing.T, row observation) {
	t.Helper()
	for key, count := range row.Actions {
		if (strings.HasSuffix(key, " GoCompilePkg") || strings.HasSuffix(key, " GoCompilePkgExternal")) && count.Executed+count.Cached != 0 {
			t.Errorf("%s: %s should be reused without execution or a disk/remote cache lookup, got %+v", row.Step, key, count)
		}
	}
}

func assertNativeCompiles(t *testing.T, row observation, want []string) {
	t.Helper()
	counts := make(map[string]int)
	for _, compile := range row.NativeCompiles {
		counts[compile.Source]++
	}
	if len(row.NativeCompiles) != len(want) {
		t.Errorf("%s: native compile spawns/cache hits = %+v, want exactly %v", row.Step, row.NativeCompiles, want)
	}
	for _, source := range want {
		if counts[source] != 1 {
			t.Errorf("%s: native compile spawns/cache hits for %s = %d, want 1", row.Step, source, counts[source])
		}
	}
}

func assertActionSkipped(t *testing.T, row observation, mnemonic string) {
	t.Helper()
	count := row.Actions["//:lib "+mnemonic]
	if count.Executed+count.Cached != 0 {
		t.Errorf("%s: %s should be reused without execution or a disk/remote cache lookup, got %+v", row.Step, mnemonic, count)
	}
}

func assertActionRefreshed(t *testing.T, row observation, mnemonic string) {
	t.Helper()
	count := row.Actions["//:lib "+mnemonic]
	if count.Executed+count.Cached == 0 {
		t.Errorf("%s: expected %s execution or a disk/remote cache hit", row.Step, mnemonic)
	}
}

func assertNoop(t *testing.T, before, after observation) {
	t.Helper()
	if len(after.Actions) != 0 {
		t.Errorf("%s: fixture spawns ran or consulted a disk/remote cache: %v", after.Step, after.Actions)
	}
	if !reflect.DeepEqual(before.SHA256, after.SHA256) {
		t.Errorf("%s: output digests changed without a source edit", after.Step)
	}
}

func readSpawns(r io.Reader) ([]spawn, error) {
	var spawns []spawn
	decoder := json.NewDecoder(r)
	for {
		var s spawn
		if err := decoder.Decode(&s); err == io.EOF {
			return spawns, nil
		} else if err != nil {
			return nil, fmt.Errorf("reading execution log: %w", err)
		}
		spawns = append(spawns, s)
	}
}

func TestReadSpawns(t *testing.T) {
	for _, tc := range []struct {
		name, log string
		want      int
		wantError bool
	}{
		{name: "noop"},
		{name: "concatenated", log: "{\n\"mnemonic\": \"GoCompilePkg\"\n}{\"mnemonic\":\"GoLink\",\"cacheHit\":true,\"metrics\":{\"totalTime\":\"0.150s\",\"executionWallTime\":\"0.120s\"}}", want: 2},
		{name: "native-input-output", log: `{"mnemonic":"GoCgoCompile","inputs":[{"path":"native.c"}],"actualOutputs":[{"path":"lib.cgo_native0.o"}]}`, want: 1},
		{name: "truncated", log: "{} {", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spawns, err := readSpawns(strings.NewReader(tc.log))
			if (err != nil) != tc.wantError {
				t.Fatalf("readSpawns error = %v, wantError = %t", err, tc.wantError)
			}
			if len(spawns) != tc.want {
				t.Fatalf("got %d spawns, want %d", len(spawns), tc.want)
			}
			if tc.name == "concatenated" && (spawns[0].Mnemonic != "GoCompilePkg" || !spawns[1].CacheHit) {
				t.Fatalf("incorrect execution/cache fields: %+v", spawns)
			}
			if tc.name == "concatenated" && (spawns[1].Metrics.TotalTime != "0.150s" || spawns[1].Metrics.ExecutionWallTime != "0.120s") {
				t.Fatalf("incorrect duration fields: %+v", spawns[1].Metrics)
			}
			if tc.name == "native-input-output" && (len(spawns[0].Inputs) != 1 || spawns[0].Inputs[0].Path != "native.c" || len(spawns[0].ActualOutputs) != 1 || spawns[0].ActualOutputs[0].Path != "lib.cgo_native0.o") {
				t.Fatalf("incorrect input/output fields: %+v", spawns[0])
			}
		})
	}
}
