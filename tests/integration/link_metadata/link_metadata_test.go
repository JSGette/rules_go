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

package_metadata(
    name = "unused_metadata",
    purl = "pkg:golang/example.com/unused@v3.0.0",
)

go_library(
    name = "dep",
    srcs = ["dep.go"],
    importpath = "example.com/dep",
    applicable_licenses = [":dep_metadata"],
)

go_library(
    name = "unused",
    srcs = ["unused.go"],
    importpath = "example.com/unused",
    applicable_licenses = [":unused_metadata"],
)

go_library(
    name = "unused_shared_metadata",
    srcs = ["shared.go"],
    importpath = "example.com/shared",
    applicable_licenses = [":dep_metadata"],
)

[go_binary(
    name = name,
    srcs = ["main.go"],
    cgo = True,
    deps = [":dep", ":unused", ":unused_shared_metadata"],
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
-- unused.go --
package unused
-- shared.go --
package shared
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
	build, outputs := observeLinks(t, targets)

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

	editFile(t, "BUILD.bazel", "@v1.0.0", "@v2.0.0", 2)
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

func TestUnusedMetadataDoesNotRelinkExecutable(t *testing.T) {
	build, outputs := observeLinks(t, []string{"//:executable"})
	if links := build("cold"); links["//:executable"] != 1 {
		t.Fatalf("cold build did not log the executable's GoLink: %v", links)
	}
	executable := outputs["//:executable"]
	before := digest(t, executable)
	if versions := readVersions(t, executable); versions["example.com/unused"] != "" {
		t.Fatalf("unimported declared dependency appears in build info: %v", versions)
	}

	// This changes the raw metadata file while all code and reachable metadata
	// stay the same. Bazel must retain the previous link without a cache lookup.
	editFile(t, "BUILD.bazel", "example.com/unused@v3.0.0", "example.com/unused@v3.1.0", 1)
	if links := build("unused-metadata"); len(links) != 0 {
		t.Fatalf("unused metadata caused a link spawn/cache lookup: %v", links)
	}
	if got := digest(t, executable); got != before {
		t.Fatal("unused metadata changed executable bytes")
	}

	// Shared metadata remains a dependency if any package using it is reached.
	editFile(t, "BUILD.bazel", "example.com/dep@v1.0.0", "example.com/dep@v1.1.0", 1)
	if links := build("shared-metadata"); links["//:executable"] != 1 {
		t.Fatalf("reachable shared metadata did not relink: %v", links)
	}
	if versions := readVersions(t, executable); versions["example.com/dep"] != "v1.1.0" {
		t.Fatalf("shared metadata version was not updated: %v", versions)
	}

	// Changing a retained imports manifest reactivates the previously pruned
	// metadata. Its latest value must be read when the link reruns.
	editFile(t, "main.go", `"example.com/dep"`, "\"example.com/dep\"\n    _ \"example.com/unused\"", 1)
	if links := build("activate-import"); links["//:executable"] != 1 {
		t.Fatalf("adding an import did not relink: %v", links)
	}
	if versions := readVersions(t, executable); versions["example.com/unused"] != "v3.1.0" {
		t.Fatalf("activated import did not read current metadata: %v", versions)
	}
	editFile(t, "BUILD.bazel", "example.com/unused@v3.1.0", "example.com/unused@v3.2.0", 1)
	if links := build("now-used-metadata"); links["//:executable"] != 1 {
		t.Fatalf("newly reachable metadata was still pruned: %v", links)
	}
	if versions := readVersions(t, executable); versions["example.com/unused"] != "v3.2.0" {
		t.Fatalf("newly reachable metadata version was not updated: %v", versions)
	}
}

func observeLinks(t *testing.T, targets []string) (func(string) map[string]int, map[string]string) {
	t.Helper()
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
			var linkedOutput string
			for _, output := range spawn.ActualOutputs {
				if strings.HasSuffix(output.Path, ".unused_buildinfo_inputs") {
					continue
				}
				if linkedOutput != "" {
					t.Fatalf("%s: multiple linked artifacts: %+v", label, spawn.ActualOutputs)
				}
				linkedOutput = output.Path
			}
			if linkedOutput == "" {
				t.Fatalf("%s: no linked artifact: %+v", label, spawn.ActualOutputs)
			}
			outputs[label] = filepath.Join(execRoot, filepath.FromSlash(linkedOutput))
		}
		return links
	}

	return build, outputs
}

func editFile(t *testing.T, path, before, after string, wantMatches int) {
	t.Helper()
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.WriteFile(path, original, 0666); err != nil {
			t.Errorf("restoring %s: %v", path, err)
		}
	})
	if count := strings.Count(string(original), before); count != wantMatches {
		t.Fatalf("%s contains %d occurrences of %q, want %d", path, count, before, wantMatches)
	}
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(string(original), before, after)), 0666); err != nil {
		t.Fatal(err)
	}
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
	versions := readVersions(t, path)
	for _, module := range []string{"example.com/main", "example.com/dep"} {
		if got := versions[module]; got != want {
			t.Errorf("module %s version = %q, want %q", module, got, want)
		}
	}
}

func readVersions(t *testing.T, path string) map[string]string {
	t.Helper()
	out, err := exec.Command(path).CombinedOutput()
	if err != nil {
		t.Fatalf("running executable: %v\n%s", err, out)
	}
	var versions map[string]string
	if err := json.Unmarshal(out, &versions); err != nil {
		t.Fatalf("decoding build info %q: %v", out, err)
	}
	return versions
}
