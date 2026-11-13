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
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestGoPackPreservesMemberOrder(t *testing.T) {
	root := t.TempDir()
	prefix := arHeader + goPackTestMember("__.PKGDEF", "exports") + goPackTestMember("_go_.o", "Go code")
	// The retained member with a duplicate name must remain after the native
	// member, just as when both are passed to pack in the combined action.
	tail := goPackTestMember("s0.o", "Go assembly") + goPackTestMember("_x3.o", "system object")
	archive := writeGoPackTestFile(t, root, "pkg.go.a", prefix+tail)
	metadata := filepath.Join(root, "pack.json")
	if err := writeGoPackMetadata(metadata, int64(len(prefix))); err != nil {
		t.Fatal(err)
	}
	objects := filepath.Join(root, "native")
	if err := os.Mkdir(objects, 0777); err != nil {
		t.Fatal(err)
	}
	prepared := cgoPrepared{}
	var wantObjects []string
	want := prefix
	for i := 0; i < 12; i++ {
		name, contents := fmt.Sprintf("_x%d.o", i), fmt.Sprintf("native object %d", i)
		prepared.ObjectFiles = append(prepared.ObjectFiles, name)
		wantObjects = append(wantObjects, writeGoPackTestFile(t, objects, name, contents))
		want += goPackTestMember(name, contents)
	}
	want += tail
	if err := writeCgoManifest(objects, prepared); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "pkg.a")
	calls := 0
	appendNative := func(path string, gotObjects []string) error {
		calls++
		if !reflect.DeepEqual(gotObjects, wantObjects) {
			t.Fatalf("native order = %v, want %v", gotObjects, wantObjects)
		}
		if got := readGoPackTestFile(t, path); got != prefix {
			t.Fatalf("archive before native pack = %q, want compiler prefix %q", got, prefix)
		}
		out, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			return err
		}
		for _, object := range gotObjects {
			if _, err := out.WriteString(goPackTestMember(filepath.Base(object), readGoPackTestFile(t, object))); err != nil {
				out.Close()
				return err
			}
		}
		return out.Close()
	}
	if err := packGoArchive(archive, metadata, objects, output, appendNative); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("native pack calls = %d, want 1", calls)
	}
	if got := readGoPackTestFile(t, output); got != want {
		t.Fatalf("combined archive differs:\ngot  %q\nwant %q", got, want)
	}
	if got := readGoPackTestFile(t, archive); got != prefix+tail {
		t.Fatal("packing mutated the Go-only archive")
	}
}

func TestGoPackNoNativeObjects(t *testing.T) {
	root := t.TempDir()
	prefix := arHeader + goPackTestMember("_go_.o", "code")
	contents := prefix + goPackTestMember("s0.o", "assembly")
	archive := writeGoPackTestFile(t, root, "pkg.go.a", contents)
	metadata := filepath.Join(root, "pack.json")
	if err := writeGoPackMetadata(metadata, int64(len(prefix))); err != nil {
		t.Fatal(err)
	}
	objects := filepath.Join(root, "native")
	if err := os.Mkdir(objects, 0777); err != nil {
		t.Fatal(err)
	}
	if err := writeCgoManifest(objects, cgoPrepared{}); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "pkg.a")
	if err := packGoArchive(archive, metadata, objects, output, func(string, []string) error {
		t.Fatal("native pack invoked without native members")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := readGoPackTestFile(t, output); got != contents {
		t.Fatal("archive with no native objects changed bytes")
	}
}

func TestGoPackArchiveValidation(t *testing.T) {
	prefix := arHeader + goPackTestMember("_go_.o", "Go code")
	archive := prefix + goPackTestMember("s0.o", "assembly")
	badSize := []byte(archive)
	copy(badSize[len(arHeader)+48:len(arHeader)+58], "garbage   ")
	negativeSize := []byte(archive)
	copy(negativeSize[len(arHeader)+48:len(arHeader)+58], "-1        ")
	badHeader := []byte(archive)
	badHeader[len(arHeader)+58] = '!'
	for _, tc := range []struct {
		name     string
		contents string
		boundary int64
		wantErr  bool
	}{
		{"compiler-prefix", archive, int64(len(prefix)), false},
		{"complete-archive", archive, int64(len(archive)), false},
		{"negative-boundary", archive, -1, true},
		{"missing-boundary", archive, 0, true},
		{"past-end", archive, int64(len(archive) + 1), true},
		{"inside-header", archive, int64(len(arHeader) + 1), true},
		{"inside-object", archive, int64(len(arHeader) + entryLength + 1), true},
		{"bad-magic", "?" + archive[1:], int64(len(prefix)), true},
		{"bad-member-size", string(badSize), int64(len(prefix)), true},
		{"negative-member-size", string(negativeSize), int64(len(prefix)), true},
		{"bad-member-header", string(badHeader), int64(len(prefix)), true},
		{"truncated-member", archive[:len(archive)-1], int64(len(prefix)), true},
		{"truncated-header", prefix + "bad header", int64(len(prefix)), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateGoPackArchive(strings.NewReader(tc.contents), int64(len(tc.contents)), tc.boundary)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateGoPackArchive error = %v, want error %t", err, tc.wantErr)
			}
		})
	}
}

func TestGoPackRejectsAliasedOutput(t *testing.T) {
	root := t.TempDir()
	input := writeGoPackTestFile(t, root, "input", "preserved")
	if err := checkGoPackOutput(input, input); err == nil {
		t.Fatal("accepted an output identical to an input")
	}
	alias := filepath.Join(root, "alias")
	if err := os.Link(input, alias); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if err := checkGoPackOutput(input, alias); err == nil {
		t.Fatal("accepted an output aliasing an input")
	}
	if got := readGoPackTestFile(t, input); got != "preserved" {
		t.Fatal("input changed during output validation")
	}
}

func goPackTestMember(name, contents string) string {
	var out bytes.Buffer
	fmt.Fprintf(&out, "%-16s%-12d%-6d%-6d%-8o%-10d`\n", name, 0, 0, 0, 0644, len(contents))
	out.WriteString(contents)
	if len(contents)%2 != 0 {
		out.WriteByte(0)
	}
	return out.String()
}

func writeGoPackTestFile(t *testing.T, dir, name, contents string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(contents), 0666); err != nil {
		t.Fatal(err)
	}
	return path
}

func readGoPackTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
