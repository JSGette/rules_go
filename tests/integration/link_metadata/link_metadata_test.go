// Copyright 2026 The Bazel Authors. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package link_metadata_test

import (
	"crypto/sha256"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bazelbuild/rules_go/go/tools/bazel_testing"
)

func TestMain(m *testing.M) {
	bazel_testing.TestMain(m, bazel_testing.Args{
		Main: `
-- BUILD.bazel --
load("@io_bazel_rules_go//go:def.bzl", "go_binary", "go_library")
load("@package_metadata//rules:package_metadata.bzl", "package_metadata")

package_metadata(
    name = "main_metadata",
    purl = "pkg:golang/example.com/main@v1.0.0",
)

package_metadata(
    name = "dep_metadata",
    purl = "pkg:golang/example.com/dep@v1.0.0",
)

go_library(
    name = "dep",
    srcs = ["dep.go"],
    importpath = "example.com/dep",
    applicable_licenses = [":dep_metadata"],
)

[go_binary(
    name = name,
    srcs = ["main.go"],
    cgo = True,
    deps = [":dep"],
    linkmode = mode,
    applicable_licenses = [":main_metadata"],
) for name, mode in [
    ("archive", "c-archive"),
    ("shared", "c-shared"),
    ("plugin", "plugin"),
    ("executable", "normal"),
]]
-- dep.go --
package dep

func Value() int { return 42 }
-- main.go --
package main

import "C"

import (
    "encoding/json"
    "os"
    "runtime/debug"
    "example.com/dep"
)

var Value = dep.Value()

func main() {
    info, ok := debug.ReadBuildInfo()
    versions := make(map[string]string)
    if ok {
        versions[info.Main.Path] = info.Main.Version
        for _, module := range info.Deps {
            versions[module.Path] = module.Version
        }
    }
    _ = json.NewEncoder(os.Stdout).Encode(versions)
}
`,
		ModuleFileSuffix: `
bazel_dep(name = "package_metadata", version = "0.0.5")
`,
	})
}

func TestMetadataDoesNotRelinkLibraries(t *testing.T) {
	switch runtime.GOOS {
	case "linux", "darwin", "freebsd", "windows":
	default:
		t.Skip("fixture requires a platform supporting both c-archive and c-shared")
	}
	targets := []string{"//:archive", "//:shared", "//:executable"}
	if runtime.GOOS != "windows" {
		targets = append(targets, "//:plugin")
	}
	// Discover outputs even if this integration test has already run against
	// bazel_testing's shared output base. Disk/remote cache hits are still valid.
	if err := bazel_testing.RunBazel("clean"); err != nil {
		t.Fatal(err)
	}
	execRootBytes, err := bazel_testing.BazelOutput("info", "execution_root")
	if err != nil {
		t.Fatal(err)
	}
	execRoot := strings.TrimSpace(string(execRootBytes))
	logDir := t.TempDir()
	outputs := make(map[string]string)

	build := func(step string) map[string]int {
		t.Helper()
		logPath := filepath.Join(logDir, step+".json")
		args := append([]string{"build"}, targets...)
		args = append(args, "--remote_download_outputs=all", "--execution_log_json_file="+logPath)
		if err := bazel_testing.RunBazel(args...); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(logPath)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		decoder := json.NewDecoder(f)
		links := make(map[string]int)
		for {
			var spawn struct {
				Mnemonic      string `json:"mnemonic"`
				TargetLabel   string `json:"targetLabel"`
				ActualOutputs []struct {
					Path string `json:"path"`
				} `json:"actualOutputs"`
			}
			if err := decoder.Decode(&spawn); err == io.EOF {
				break
			} else if err != nil {
				t.Fatal(err)
			}
			if spawn.Mnemonic != "GoLink" {
				continue
			}
			label := strings.TrimPrefix(spawn.TargetLabel, "@@")
			links[label]++
			if len(spawn.ActualOutputs) != 1 {
				t.Fatalf("%s: GoLink outputs = %+v, want one linked artifact", label, spawn.ActualOutputs)
			}
			outputs[label] = filepath.Join(execRoot, filepath.FromSlash(spawn.ActualOutputs[0].Path))
		}
		return links
	}

	cold := build("cold")
	before := make(map[string][sha256.Size]byte)
	for _, target := range targets {
		if cold[target] != 1 {
			t.Fatalf("cold build logged %d GoLink spawns for %s, want one", cold[target], target)
		}
		before[target] = digest(t, outputs[target])
	}
	checkVersions(t, outputs["//:executable"], "v1.0.0")
	if links := build("noop"); len(links) != 0 {
		t.Fatalf("no-op build logged GoLink spawns: %v", links)
	}

	original, err := os.ReadFile("BUILD.bazel")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.WriteFile("BUILD.bazel", original, 0666); err != nil {
			t.Errorf("restoring metadata: %v", err)
		}
	})
	if count := strings.Count(string(original), "@v1.0.0"); count != 2 {
		t.Fatalf("fixture contains %d metadata versions, want two", count)
	}
	if err := os.WriteFile("BUILD.bazel", []byte(strings.ReplaceAll(string(original), "@v1.0.0", "@v2.0.0")), 0666); err != nil {
		t.Fatal(err)
	}
	changed := build("metadata")
	for _, target := range targets {
		after := digest(t, outputs[target])
		if target == "//:executable" {
			if changed[target] != 1 || before[target] == after {
				t.Errorf("executable must relink with changed metadata: spawns=%d, identical=%t", changed[target], before[target] == after)
			}
		} else if changed[target] != 0 || before[target] != after {
			t.Errorf("%s should reuse its output after metadata edits: spawns=%d, identical=%t", target, changed[target], before[target] == after)
		}
	}
	checkVersions(t, outputs["//:executable"], "v2.0.0")
}

func digest(t *testing.T, path string) [sha256.Size]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(data)
}

func checkVersions(t *testing.T, path, want string) {
	t.Helper()
	out, err := exec.Command(path).CombinedOutput()
	if err != nil {
		t.Fatalf("running executable: %v\n%s", err, out)
	}
	var versions map[string]string
	if err := json.Unmarshal(out, &versions); err != nil {
		t.Fatalf("decoding build info %q: %v", out, err)
	}
	for _, module := range []string{"example.com/main", "example.com/dep"} {
		if got := versions[module]; got != want {
			t.Errorf("module %s version = %q, want %q", module, got, want)
		}
	}
}
