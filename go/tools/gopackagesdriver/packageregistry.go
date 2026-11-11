// Copyright 2021 The Bazel Authors. All rights reserved.
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
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

type PackageRegistry struct {
	packagesByID map[string]*packages.Package
	stdlib       map[string]*packages.Package
}

func NewPackageRegistry(pkgs ...*FlatPackage) *PackageRegistry {
	pr := &PackageRegistry{
		packagesByID: map[string]*packages.Package{},
		stdlib:       map[string]*packages.Package{},
	}
	pr.Add(pkgs...)
	return pr
}

func (pr *PackageRegistry) Add(pkgs ...*FlatPackage) *PackageRegistry {
	for _, flatPkg := range pkgs {
		imports := make(map[string]*packages.Package)
		for impKey, imp := range flatPkg.Imports {
			imports[impKey] = &packages.Package{
				ID: imp,
			}
		}

		pkg := &packages.Package{
			ID:              flatPkg.ID,
			Name:            flatPkg.Name,
			PkgPath:         flatPkg.PkgPath,
			GoFiles:         flatPkg.GoFiles,
			CompiledGoFiles: flatPkg.CompiledGoFiles,
			OtherFiles:      flatPkg.OtherFiles,
			ExportFile:      flatPkg.ExportFile,
			Imports:         imports,
		}

		if len(pkg.CompiledGoFiles) <= 0 {
			pkg.CompiledGoFiles = pkg.GoFiles
		}

		pr.packagesByID[pkg.ID] = pkg

		if flatPkg.IsStdlib() {
			pr.stdlib[pkg.PkgPath] = pkg
		}
	}
	return pr
}

func (pr *PackageRegistry) ResolvePaths(prf PathResolverFunc) error {
	for _, pkg := range pr.packagesByID {
		ResolvePaths(pkg, prf)
		FilterFilesForBuildTags(pkg)
	}
	return nil
}

// ResolveImports adds stdlib imports to packages. This is required because
// stdlib packages are not part of the JSON file exports as bazel is unaware of
// them.
func (pr *PackageRegistry) ResolveImports(overlays map[string][]byte) error {
	resolve := func(importPath string) *packages.Package {
		if pkg, ok := pr.stdlib[importPath]; ok {
			return pkg
		}

		return nil
	}

	for _, pkg := range pr.packagesByID {
		if err := ResolveImports(pkg, resolve, overlays); err != nil {
			return err
		}

		testPkg := MoveTestFiles(pkg)
		if testPkg != nil {
			pr.packagesByID[testPkg.ID] = testPkg
		}
	}

	return nil
}

func (pr *PackageRegistry) walk(acc map[string]*packages.Package, root string) {
	pkg := pr.packagesByID[root]

	if pkg == nil {
		return
	}

	acc[pkg.ID] = pkg
	for _, pkgI := range pkg.Imports {
		if _, ok := acc[pkgI.ID]; !ok {
			pr.walk(acc, pkgI.ID)
		}
	}
}

// MatchPatterns maps driver queries onto package IDs already in the registry.
// Relative file= paths and local patterns resolve against root. Stdlib
// packages are roots only for "std" and "builtin": with rules_go as the main
// module their IDs are "@@//stdlib:...", which "./..." would otherwise match.
func (pr *PackageRegistry) MatchPatterns(queries []string, root string) ([]string, error) {
	var files map[string][]string
	ids := make([]string, 0, len(queries))
	seen := map[string]struct{}{}

	for _, query := range queries {
		var (
			matched []string
			err     error
		)
		switch {
		case isFileQuery(query):
			if files == nil {
				files = pr.fileIndex(root)
			}
			matched, err = matchFile(files, query, root)
		case query == "std" || query == "builtin":
			matched, err = pr.matchStdlib(query)
		case isLocalPattern(query):
			matched, err = pr.matchLocal(query, root)
		default:
			matched, err = pr.matchImportPath(query)
		}
		if err != nil {
			return nil, err
		}
		for _, id := range matched {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}

	sort.Strings(ids)
	return ids, nil
}

func (pr *PackageRegistry) matchImportPath(query string) ([]string, error) {
	var (
		exact  string
		prefix string
	)
	if strings.HasSuffix(query, "/...") {
		prefix = strings.TrimSuffix(query, "/...")
	} else {
		exact = query
	}

	var ids []string
	for _, pkg := range pr.packagesByID {
		if pr.isStdlib(pkg) {
			continue
		}
		if exact != "" {
			if pkg.PkgPath != exact {
				continue
			}
		} else if pkg.PkgPath != prefix && !strings.HasPrefix(pkg.PkgPath, prefix+"/") {
			continue
		}
		ids = append(ids, pkg.ID)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("found no packages matching import path %q", query)
	}
	return ids, nil
}

func (pr *PackageRegistry) matchStdlib(query string) ([]string, error) {
	var ids []string
	for _, pkg := range pr.stdlib {
		if query == "std" || pkg.PkgPath == query {
			ids = append(ids, pkg.ID)
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("found no packages matching %q", query)
	}
	return ids, nil
}

func (pr *PackageRegistry) matchLocal(query, root string) ([]string, error) {
	dir, recursive, err := localPatternDir(query, root)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, pkg := range pr.packagesByID {
		if pr.isStdlib(pkg) {
			continue
		}
		pkgDir, ok := mainRepoPackageDir(pkg.ID)
		if !ok {
			continue
		}
		if pkgDir == dir || (recursive && (dir == "" || strings.HasPrefix(pkgDir, dir+"/"))) {
			ids = append(ids, pkg.ID)
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("found no packages matching %q", query)
	}
	return ids, nil
}

func matchFile(files map[string][]string, query, root string) ([]string, error) {
	ids := files[absPath(strings.TrimPrefix(query, "file="), root)]
	if len(ids) == 0 {
		return nil, fmt.Errorf("found no packages matching file %q", query)
	}
	return ids, nil
}

func (pr *PackageRegistry) fileIndex(root string) map[string][]string {
	idx := map[string][]string{}
	for _, pkg := range pr.packagesByID {
		for _, files := range [][]string{pkg.GoFiles, pkg.CompiledGoFiles, pkg.OtherFiles} {
			for _, f := range files {
				key := absPath(f, root)
				idx[key] = append(idx[key], pkg.ID)
			}
		}
	}
	return idx
}

func (pr *PackageRegistry) isStdlib(pkg *packages.Package) bool {
	_, ok := pr.stdlib[pkg.PkgPath]
	return ok
}

func isFileQuery(query string) bool {
	return strings.HasPrefix(query, "file=") || (isLocalPattern(query) && strings.HasSuffix(query, ".go"))
}

func absPath(p, root string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(root, p)
}

// localPatternDir returns the slash-separated directory of a local pattern
// relative to root, with "" for root itself.
func localPatternDir(pattern, root string) (string, bool, error) {
	dir, recursive := strings.CutSuffix(pattern, "...")
	if filepath.IsAbs(dir) {
		rel, err := filepath.Rel(root, dir)
		if err != nil {
			return "", false, fmt.Errorf("query %q is outside %s", pattern, root)
		}
		dir = rel
	}
	dir = path.Clean(filepath.ToSlash(dir))
	if dir == ".." || strings.HasPrefix(dir, "../") {
		return "", false, fmt.Errorf("query %q is outside %s", pattern, root)
	}
	if dir == "." {
		dir = ""
	}
	return dir, recursive, nil
}

// mainRepoPackageDir returns the package of a main-repo label such as
// "@@//pkg:name". Labels in other repos, such as "@@rules_go+//pkg:name", are
// never matched by local patterns.
func mainRepoPackageDir(id string) (string, bool) {
	repo, rest, ok := strings.Cut(id, "//")
	if !ok || (repo != "" && repo != "@" && repo != "@@") {
		return "", false
	}
	pkg, _, ok := strings.Cut(rest, ":")
	return pkg, ok
}

func (pr *PackageRegistry) Match(labels []string) ([]string, []*packages.Package) {
	return pr.match(labels, true)
}

// MatchCanonicalIDs is like Match but does not prefix @ onto IDs.
// Use it when IDs already come from .pkg.json.
func (pr *PackageRegistry) MatchCanonicalIDs(ids []string) ([]string, []*packages.Package) {
	return pr.match(ids, false)
}

func (pr *PackageRegistry) match(labels []string, canonicalize bool) ([]string, []*packages.Package) {
	roots := map[string]struct{}{}

	for _, label := range labels {
		// When packagesdriver is ran from rules go, rulesGoRepositoryName will just be @
		if canonicalize && !strings.HasPrefix(label, "@") {
			label = fmt.Sprintf("@%s", label)
		}

		if label == RulesGoStdlibLabel {
			// For stdlib, we need to append all the subpackages as roots
			// since RulesGoStdLibLabel doesn't actually show up in the stdlib pkg.json
			for _, pkg := range pr.stdlib {
				roots[pkg.ID] = struct{}{}
			}
		} else if _, ok := pr.packagesByID[label]; ok {
			roots[label] = struct{}{}
			// If an xtest package exists for this package add it to the roots
			if _, ok := pr.packagesByID[label+"_xtest"]; ok {
				roots[label+"_xtest"] = struct{}{}
			}
		} else {
			// Skip a package if we don't have .pkg.json for it.
			// This happens if 'bazel query' matches the target, but 'bazel build'
			// can't analyze it, for example, if target_compatible_with is set
			// with contraints not compatible with the host platform.
			continue
		}
	}

	walkedPackages := map[string]*packages.Package{}
	retRoots := make([]string, 0, len(roots))
	for rootPkg := range roots {
		retRoots = append(retRoots, rootPkg)
		pr.walk(walkedPackages, rootPkg)
	}

	retPkgs := make([]*packages.Package, 0, len(walkedPackages))
	for _, pkg := range walkedPackages {
		retPkgs = append(retPkgs, pkg)
	}

	return retRoots, retPkgs
}
