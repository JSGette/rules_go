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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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
    srcs = ["ordinary.go", "nested/bridge.go", "native.c", "native.h"],
    cdeps = [":cdep"],
    cgo = True,
    copts = ["-DNATIVE_OPTION=0"],
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
-- native.c --
#include "native.h"
#include "cdep/native.h"
int native(void) { return 30 + HEADER_VALUE + NATIVE_OPTION + cdep(); }
-- native.h --
#define HEADER_VALUE 40
int native(void);
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
`})
}

// These are the fields needed from Bazel's SpawnExec proto. The JSON execution
// log is a stream of concatenated messages, not a JSON array or one-line JSON.
// See https://github.com/bazelbuild/bazel/blob/9.3.1/src/main/protobuf/spawn.proto.
type spawn struct {
	Mnemonic      string `json:"mnemonic"`
	TargetLabel   string `json:"targetLabel"`
	Runner        string `json:"runner"`
	CacheHit      bool   `json:"cacheHit"`
	ActualOutputs []struct {
		Path string `json:"path"`
	} `json:"actualOutputs"`
}

type actionCount struct {
	Executed int `json:"executed"`
	Cached   int `json:"cached"`
}

type observation struct {
	Variant       string                 `json:"variant"`
	Step          string                 `json:"step"`
	ElapsedMillis int64                  `json:"elapsedMillis"`
	Actions       map[string]actionCount `json:"actions"`
	SHA256        map[string]string      `json:"sha256"`
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
	var executable string

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
			for _, output := range s.ActualOutputs {
				if s.Mnemonic == "GoCgoPrepare" {
					preparedArtifacts[output.Path] = true
				}
				if s.Mnemonic == "GoCgoPrepare" || strings.HasSuffix(output.Path, ".a") || strings.HasSuffix(output.Path, ".x") {
					artifacts[output.Path] = label
				}
				if label == "//:app" && s.Mnemonic == "GoLink" {
					executable = output.Path
					artifacts[output.Path] = label
				}
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
		assertActionRefreshed(t, baseline, "GoCgoPrepare")
		if len(preparedArtifacts) == 0 {
			t.Fatal("baseline did not log prepared cgo outputs")
		}
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
		{"header", "native.h", "HEADER_VALUE 40", "HEADER_VALUE 41", "//:lib", false, true},
		{"go-dependency-body", "go_dep/dep.go", "return 0", "return 1", "//:go_dep", true, false},
		// cdeps are linked separately: a body edit refreshes preparation but
		// need not change its objects or the containing Go package's archive.
		{"cdep-body", "cdep/native.c", "return 0", "return 1", "//:cdep", false, false},
		{"copts", "BUILD.bazel", "-DNATIVE_OPTION=0", "-DNATIVE_OPTION=1", "//:lib", false, true},
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
		{name: "concatenated", log: "{\n\"mnemonic\": \"GoCompilePkg\"\n}{\"mnemonic\":\"GoLink\",\"cacheHit\":true}", want: 2},
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
		})
	}
}
