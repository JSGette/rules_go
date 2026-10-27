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
	"path/filepath"
	"strings"
	"testing"
)

func TestMatchPatterns(t *testing.T) {
	root := t.TempDir()
	pr := NewPackageRegistry(
		&FlatPackage{ID: "//:root", Name: "root", PkgPath: "example.com/root"},
		&FlatPackage{ID: "//lib:go_default_library", Name: "lib", PkgPath: "example.com/lib", GoFiles: []string{"lib/lib.go"}},
		&FlatPackage{ID: "//lib:lib_test", Name: "lib_test", PkgPath: "example.com/lib_test", GoFiles: []string{"lib/lib.go", "lib/lib_test.go"}},
		&FlatPackage{ID: "//lib/sub:go_default_library", Name: "sub", PkgPath: "example.com/lib/sub"},
		&FlatPackage{ID: "//other:go_default_library", Name: "other", PkgPath: "example.com/other"},
		&FlatPackage{ID: "//tool:tool", Name: "tool", PkgPath: "example.com/tool.go"},
		&FlatPackage{ID: "@@rules_go+//lib:external", Name: "external", PkgPath: "example.com/external"},
		&FlatPackage{ID: "//stdlib:builtin", Name: "builtin", PkgPath: "builtin", Standard: true},
		&FlatPackage{ID: "//stdlib:fmt", Name: "fmt", PkgPath: "fmt", Standard: true},
		&FlatPackage{ID: "@@//stdlib:net/http", Name: "http", PkgPath: "net/http", Standard: true},
	)

	const libPkgs = "//lib:go_default_library //lib:lib_test"
	for _, tc := range []struct {
		name    string
		queries []string
		want    string
		wantErr string
	}{
		{name: "exact", queries: []string{"example.com/lib"}, want: "//lib:go_default_library"},
		{name: "prefix", queries: []string{"example.com/lib/..."}, want: "//lib/sub:go_default_library //lib:go_default_library"},
		{name: "unknown", queries: []string{"example.com/missing"}, wantErr: "found no packages matching import path"},
		{name: "stdlib_import_path_not_root", queries: []string{"fmt"}, wantErr: "found no packages matching import path"},
		{name: "import_path_ending_in_go", queries: []string{"example.com/tool.go"}, want: "//tool:tool"},
		{name: "std", queries: []string{"std"}, want: "//stdlib:builtin //stdlib:fmt @@//stdlib:net/http"},
		{name: "builtin", queries: []string{"builtin"}, want: "//stdlib:builtin"},
		{name: "file", queries: []string{"file=lib/lib.go"}, want: libPkgs},
		{name: "file_absolute", queries: []string{"file=" + filepath.Join(root, "lib", "lib.go")}, want: libPkgs},
		{name: "local_file", queries: []string{"./lib/lib_test.go"}, want: "//lib:lib_test"},
		{name: "file_miss", queries: []string{"file=missing.go"}, wantErr: "found no packages matching file"},
		{name: "local_package", queries: []string{"./lib"}, want: libPkgs},
		{name: "local_recursive", queries: []string{"./lib/..."}, want: "//lib/sub:go_default_library " + libPkgs},
		{name: "local_root_skips_stdlib_and_external", queries: []string{"./..."}, want: "//:root //lib/sub:go_default_library " + libPkgs + " //other:go_default_library //tool:tool"},
		{name: "dot", queries: []string{"."}, want: "//:root"},
		{name: "local_absolute", queries: []string{filepath.Join(root, "lib")}, want: libPkgs},
		{name: "local_outside_root", queries: []string{"../.."}, wantErr: "is outside"},
		{name: "deduplicates", queries: []string{"./lib", "file=lib/lib.go"}, want: libPkgs},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ids, err := pr.MatchPatterns(tc.queries, root)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("got %v, want error containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(ids, " "); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
