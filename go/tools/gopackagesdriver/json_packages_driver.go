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

	"golang.org/x/tools/go/packages"
)

type JSONPackagesDriver struct {
	registry *PackageRegistry
}

func NewJSONPackagesDriver(jsonFiles []string, prf PathResolverFunc, overlays map[string][]byte) (*JSONPackagesDriver, error) {
	jpd := &JSONPackagesDriver{
		registry: NewPackageRegistry(),
	}

	for _, f := range jsonFiles {
		if err := WalkFlatPackagesFromJSON(f, func(pkg *FlatPackage) {
			jpd.registry.Add(pkg)
		}); err != nil {
			return nil, fmt.Errorf("unable to walk json: %w", err)
		}
	}

	if err := jpd.registry.ResolvePaths(prf); err != nil {
		return nil, fmt.Errorf("unable to resolve paths: %w", err)
	}

	if err := jpd.registry.ResolveImports(overlays); err != nil {
		return nil, fmt.Errorf("unable to resolve imports: %w", err)
	}

	return jpd, nil
}

func (b *JSONPackagesDriver) GetResponse(labels []string) *packages.DriverResponse {
	roots, pkgs := b.registry.Match(labels)
	return b.driverResponse(roots, pkgs)
}

// GetResponseFromIDs builds a driver response from package IDs already known
// to be canonical (e.g. from .pkg.json), without Bazel version label rewriting.
func (b *JSONPackagesDriver) GetResponseFromIDs(ids []string) *packages.DriverResponse {
	roots, pkgs := b.registry.MatchCanonicalIDs(ids)
	return b.driverResponse(roots, pkgs)
}

func (b *JSONPackagesDriver) driverResponse(roots []string, pkgs []*packages.Package) *packages.DriverResponse {
	return &packages.DriverResponse{
		NotHandled: false,
		Compiler:   buildContext.Compiler,
		Arch:       buildContext.GOARCH,
		Roots:      roots,
		Packages:   pkgs,
	}
}
