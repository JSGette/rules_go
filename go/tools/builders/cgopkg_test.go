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

package main

import (
	"go/build"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCgoProjectionOrdinaryGoEdit(t *testing.T) {
	root := t.TempDir()
	bridge := writeCgoTestSource(t, root, "bridge.go", "package p\n/* int answer(void); */\nimport \"C\"\nfunc Native() int { return int(C.answer()) }\n")
	ordinary := writeCgoTestSource(t, root, "logic.go", "package p\nfunc Answer() int { return Native() + 1 }\n")
	sources := []string{ordinary, bridge}
	first := makeCgoTestProjection(t, root, sources, "off")
	writeCgoTestSource(t, root, "logic.go", "package p\nfunc Answer() int { return Native() + 2 }\n")
	second := makeCgoTestProjection(t, root, sources, "off")
	if got, want := cgoTestTree(t, second), cgoTestTree(t, first); !reflect.DeepEqual(got, want) {
		t.Fatalf("ordinary Go edit changed cgo projection:\ngot %v\nwant %v", got, want)
	}
	// A source changing classification must invalidate the native action.
	writeCgoTestSource(t, root, "logic.go", "package p\nimport \"C\"\nfunc Answer() int { return Native() + 2 }\n")
	third := makeCgoTestProjection(t, root, sources, "off")
	if reflect.DeepEqual(cgoTestTree(t, second), cgoTestTree(t, third)) {
		t.Fatal("adding import C did not change the cgo projection")
	}
	// The projected files must not link back to the original inputs.
	if err := os.Remove(bridge); err != nil {
		t.Fatal(err)
	}
	if got, want := cgoTestTree(t, first), cgoTestTree(t, second); !reflect.DeepEqual(got, want) {
		t.Fatal("projection depends on the original input files")
	}
}

func TestCgoProjectionBuildAndTestFilters(t *testing.T) {
	root := t.TempDir()
	bridge := writeCgoTestSource(t, root, "bridge.go", "package p\nimport \"C\"\n")
	ignored := writeCgoTestSource(t, root, "ignored.go", "//go:build ignore\n\npackage p\nimport \"C\"\n")
	external := writeCgoTestSource(t, root, "external_test.go", "package p_test\nimport \"C\"\n")
	sources := []string{bridge, ignored, external}
	for _, tc := range []struct {
		filter, wantPackage, wantFile string
	}{{"exclude", "p", "bridge.go"}, {"only", "p_test", "external_test.go"}} {
		t.Run(tc.filter, func(t *testing.T) {
			out := makeCgoTestProjection(t, root, sources, tc.filter)
			var projection cgoProjection
			if err := readCgoManifest(out, &projection); err != nil {
				t.Fatal(err)
			}
			if projection.PackageName != tc.wantPackage || len(projection.Sources) != 1 || projection.Sources[0].Original != tc.wantFile {
				t.Fatalf("unexpected projection: %+v", projection)
			}
		})
	}
}

func TestCgoProjectionSourcePaths(t *testing.T) {
	var trees []map[string]string
	for i := 0; i < 2; i++ {
		top := t.TempDir()
		root := filepath.Join(top, "execroot")
		// Cover generated files, duplicate basenames in distinct directories,
		// and a sibling external repository without escaping the output tree.
		sources := []string{
			writeCgoTestSource(t, root, "nested/bridge.go", "package p\nimport \"C\"\n//line original.go:73\nfunc A() {}\n"),
			writeCgoTestSource(t, root, "nested/other.go", "package p\nimport \"C\"\n"),
			writeCgoTestSource(t, root, "bazel-out/bin/gen/bridge.go", "package p\nimport \"C\"\n"),
			writeCgoTestSource(t, top, "external/bridge.go", "package p\nimport \"C\"\n"),
		}
		out := makeCgoTestProjection(t, root, sources, "off")
		trees = append(trees, cgoTestTree(t, out))
		var projection cgoProjection
		if err := readCgoManifest(out, &projection); err != nil {
			t.Fatal(err)
		}
		projected, rewrites, err := projection.inputs(out)
		if err != nil {
			t.Fatal(err)
		}
		if len(projected) != 4 || len(rewrites) != 3 {
			t.Fatalf("got %d inputs, %d source directories; want 4, 3", len(projected), len(rewrites))
		}
		if filepath.Dir(projected[0]) != filepath.Dir(projected[1]) || filepath.Dir(projected[0]) == filepath.Dir(projected[2]) {
			t.Fatal("projection did not preserve source-directory grouping")
		}
		wantRewrite := filepath.Join(out, "src", "2") + "=>" + filepath.Join("..", "external")
		if rewrites[2] != wantRewrite {
			t.Fatalf("sibling source rewrite = %q, want %q", rewrites[2], wantRewrite)
		}
		for _, path := range projected {
			if !strings.HasPrefix(path, out+string(filepath.Separator)) {
				t.Fatalf("projected path escapes output directory: %s", path)
			}
		}
	}
	if !reflect.DeepEqual(trees[0], trees[1]) {
		t.Fatalf("projection is not independent of execroot:\n%v\n%v", trees[0], trees[1])
	}
}

func TestCgoPrepareEmpty(t *testing.T) {
	t.Setenv("CGO_ENABLED", "1")
	root := t.TempDir()
	projection := filepath.Join(root, "projection")
	if err := writeCgoProjection(nil, projection, root); err != nil {
		t.Fatal(err)
	}
	goDir, objectDir := filepath.Join(root, "go"), filepath.Join(root, "objects")
	header := filepath.Join(root, "export.h")
	if err := cgoprepare([]string{
		"-sdk", root, "-projection", projection,
		"-cgo_go_srcs", goDir, "-cgo_objects", objectDir, "-cgoexport", header,
	}); err != nil {
		t.Fatal(err)
	}
	goSrcs, objects, err := readCgoPrepared(goDir, objectDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(goSrcs)+len(objects) != 0 {
		t.Fatalf("empty package generated Go sources %v or native objects %v", goSrcs, objects)
	}
	for _, path := range []string{goDir, objectDir, header} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("declared output %s missing: %v", path, err)
		}
	}
}

func TestReadCgoPreparedOrder(t *testing.T) {
	root := t.TempDir()
	prepared := cgoPrepared{
		GoFiles:     []string{"_cgo_gotypes.go", "bridge.cgo1.go", "_cgo_imports.go"},
		ObjectFiles: []string{"_x0.o", "_x2.o", "_x10.o"},
	}
	if err := writeCgoManifest(root, prepared); err != nil {
		t.Fatal(err)
	}
	goSrcs, objects, err := readCgoPrepared(root, root)
	if err != nil {
		t.Fatal(err)
	}
	for i, got := range goSrcs {
		if want := filepath.Join(root, prepared.GoFiles[i]); got != want {
			t.Errorf("Go source %d = %q, want %q", i, got, want)
		}
	}
	for i, got := range objects {
		if want := filepath.Join(root, prepared.ObjectFiles[i]); got != want {
			t.Errorf("object %d = %q, want %q", i, got, want)
		}
	}
	prepared.ObjectFiles = []string{"../outside.o"}
	if err := writeCgoManifest(root, prepared); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readCgoPrepared(root, root); err == nil {
		t.Error("accepted an object path outside the prepared output")
	}
}

func makeCgoTestProjection(t *testing.T, root string, sources []string, testFilter string) string {
	t.Helper()
	oldContext := build.Default
	build.Default.CgoEnabled = true
	build.Default.BuildTags = nil
	defer func() { build.Default = oldContext }()
	srcs, err := filterAndSplitFiles(sources)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyTestFilter(testFilter, &srcs); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := writeCgoProjection(srcs.goSrcs, out, root); err != nil {
		t.Fatal(err)
	}
	return out
}

func writeCgoTestSource(t *testing.T, root, name, contents string) string {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0666); err != nil {
		t.Fatal(err)
	}
	return path
}

func cgoTestTree(t *testing.T, root string) map[string]string {
	t.Helper()
	files := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			t.Errorf("projection contains a symlink: %s", path)
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(relative)] = string(contents)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}
