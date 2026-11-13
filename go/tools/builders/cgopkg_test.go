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
	nativeManifest := filepath.Join(root, "native.json")
	if err := writeCgoNativePlan(archiveSrcs{}, nativeManifest, root); err != nil {
		t.Fatal(err)
	}
	generated := filepath.Join(root, "generated")
	header, installHeader := filepath.Join(root, "_cgo_export.h"), filepath.Join(root, "_cgo_install.h")
	if err := cgogenerate([]string{
		"-sdk", root, "-projection", projection, "-native_manifest", nativeManifest,
		"-out", generated, "-cgoexport", header, "-cgo_install_header", installHeader,
	}); err != nil {
		t.Fatal(err)
	}
	goDir, objectDir := filepath.Join(root, "go"), filepath.Join(root, "objects")
	if err := cgoprepare([]string{
		"-sdk", root, "-generated", generated, "-native_manifest", nativeManifest,
		"-cgo_go_srcs", goDir, "-cgo_objects", objectDir,
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
	for _, path := range []string{goDir, objectDir, header, installHeader} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("declared output %s missing: %v", path, err)
		}
	}
}

func TestCgoNativePlanBodyAndTags(t *testing.T) {
	root := t.TempDir()
	bridge := writeCgoTestSource(t, root, "bridge.go", "package p\nimport \"C\"\n")
	native := writeCgoTestSource(t, root, "native.c", "int answer(void) { return 1; }\n")
	cxx := writeCgoTestSource(t, root, "other.cc", "int other() { return 2; }\n")
	asm := writeCgoTestSource(t, root, "native.S", "// assembly\n")
	header := writeCgoTestSource(t, root, "native.h", "int answer(void);\n")
	ignored := writeCgoTestSource(t, root, "ignored.c", "//go:build ignore\n\nint ignored;\n")
	// Start with a different input order to exercise language ordering.
	sources := []string{asm, cxx, native, ignored, header, bridge}
	first, firstFile := makeCgoTestNativePlan(t, root, sources)
	want := cgoNativePlan{
		Sources: []cgoNativeSource{{Path: "native.c", Kind: "c"}, {Path: "other.cc", Kind: "cxx"}, {Path: "native.S", Kind: "asm"}},
		Headers: []string{"native.h"}, HaveCgo: true, HasImportC: true,
	}
	if !reflect.DeepEqual(first, want) {
		t.Fatalf("native plan = %+v, want %+v", first, want)
	}
	writeCgoTestSource(t, root, "native.c", "int answer(void) { return 3; }\n")
	_, secondFile := makeCgoTestNativePlan(t, root, sources)
	firstBytes, err := os.ReadFile(firstFile)
	if err != nil {
		t.Fatal(err)
	}
	secondBytes, err := os.ReadFile(secondFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstBytes) != string(secondBytes) {
		t.Fatalf("native body edit changed classification output:\n%s\n%s", firstBytes, secondBytes)
	}
	writeCgoTestSource(t, root, "native.c", "//go:build ignore\n\nint answer(void) { return 3; }\n")
	filtered, _ := makeCgoTestNativePlan(t, root, sources)
	if len(filtered.Sources) != 2 || filtered.Sources[0].Kind != "cxx" {
		t.Fatalf("native build-tag change did not update selection: %+v", filtered)
	}
	// Assembly alone remains the responsibility of the Go assembler.
	asmOnly, _ := makeCgoTestNativePlan(t, root, []string{asm})
	if asmOnly.HaveCgo || asmOnly.HasImportC || len(asmOnly.Sources) != 1 {
		t.Fatalf("assembly-only package incorrectly uses native compilation: %+v", asmOnly)
	}
}

func TestCgoCompileInactiveSource(t *testing.T) {
	t.Setenv("CGO_ENABLED", "1")
	t.Setenv("CC", "")
	for _, tc := range []struct {
		name string
		plan cgoNativePlan
	}{
		{"filtered", cgoNativePlan{HaveCgo: true}},
		{"go_assembly", cgoNativePlan{Sources: []cgoNativeSource{{Path: "missing.S", Kind: "asm"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			manifest := filepath.Join(root, "native.json")
			if err := writeCgoJSON(manifest, tc.plan); err != nil {
				t.Fatal(err)
			}
			object := filepath.Join(root, "native.o")
			if err := cgocompile([]string{"-sdk", root, "-native_manifest", manifest,
				"-src", "missing.S", "-cgoexport", filepath.Join(root, "missing.h"), "-o", object}); err != nil {
				t.Fatal(err)
			}
			contents, err := os.ReadFile(object)
			if err != nil || len(contents) != 0 {
				t.Fatalf("inactive source did not produce an empty placeholder: %q, %v", contents, err)
			}
		})
	}
}

func TestCgoPrepareNativeOrderWithoutSources(t *testing.T) {
	root := t.TempDir()
	generated := filepath.Join(root, "generated")
	if err := os.MkdirAll(generated, 0777); err != nil {
		t.Fatal(err)
	}
	if err := writeCgoManifest(generated, cgoGenerated{}); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(root, "native.json")
	plan := cgoNativePlan{
		HaveCgo: true,
		Sources: []cgoNativeSource{{Path: "missing=a.c", Kind: "c"}, {Path: "missing.cc", Kind: "cxx"}, {Path: "missing.S", Kind: "asm"}},
	}
	if err := writeCgoJSON(manifest, plan); err != nil {
		t.Fatal(err)
	}
	goDir, objectDir := filepath.Join(root, "go"), filepath.Join(root, "objects")
	args := []string{"-sdk", root, "-generated", generated, "-native_manifest", manifest,
		"-cgo_go_srcs", goDir, "-cgo_objects", objectDir}
	// The original sources do not exist. Finishing must read only object
	// files, use the plan's order, and omit an excluded source's placeholder.
	for _, input := range []struct{ source, contents string }{
		{"missing.S", "assembly object"}, {"excluded.c", ""}, {"missing.cc", "C++ object"}, {"missing=a.c", "C object"},
	} {
		object := writeCgoTestSource(t, root, input.source+".o", input.contents)
		args = append(args, "-native_src", input.source, "-native_object", object)
	}
	if err := cgoprepare(args); err != nil {
		t.Fatal(err)
	}
	goSrcs, objects, err := readCgoPrepared(goDir, objectDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(goSrcs) != 0 || len(objects) != 3 {
		t.Fatalf("unexpected prepared outputs: %v, %v", goSrcs, objects)
	}
	for i, want := range []string{"C object", "C++ object", "assembly object"} {
		got, err := os.ReadFile(objects[i])
		if err != nil || string(got) != want {
			t.Errorf("object %d = %q (%v), want %q", i, got, err, want)
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

func makeCgoTestNativePlan(t *testing.T, root string, sources []string) (cgoNativePlan, string) {
	t.Helper()
	oldContext := build.Default
	build.Default.CgoEnabled = true
	build.Default.BuildTags = nil
	defer func() { build.Default = oldContext }()
	srcs, err := filterAndSplitFiles(sources)
	if err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(t.TempDir(), "native.json")
	if err := writeCgoNativePlan(srcs, manifest, root); err != nil {
		t.Fatal(err)
	}
	var plan cgoNativePlan
	if err := readCgoJSON(manifest, &plan); err != nil {
		t.Fatal(err)
	}
	return plan, manifest
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
