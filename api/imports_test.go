// Copyright (C) 2026 The OpenEverest Contributors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package api_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// moduleRoot is the import path of this module.
const moduleRoot = "github.com/openeverest/monitoring-pmm/api"

// TestImportsStayCheap keeps the contract cheap to depend on: providers
// import it, so it imports nothing from OpenEverest, controller-runtime or
// client-go. resource.Quantity is allowed, since the apiextensions v1 types
// import it already. Tests may import anything.
func TestImportsStayCheap(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	files := 0
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		files++
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range file.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			assert.True(t, allowedImport(p), "%s imports %s", path, p)
		}
		return nil
	})
	require.NoError(t, err)
	require.NotZero(t, files)
}

// allowedImport reports whether the contract may import path: the standard
// library, this module, the API types it is written with and their encoding.
func allowedImport(path string) bool {
	allowed := []string{
		moduleRoot,
		"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1",
		"k8s.io/apimachinery/pkg/api/resource",
		"sigs.k8s.io/yaml",
	}
	first, _, _ := strings.Cut(path, "/")
	if !strings.Contains(first, ".") {
		return true // the standard library
	}
	for _, a := range allowed {
		if path == a || strings.HasPrefix(path, a+"/") {
			return true
		}
	}
	return false
}

// TestModuleRequiresNoController keeps OpenEverest core and the controller
// libraries out of the module graph providers inherit. The class generator
// runs with go run pkg@version, which needs no requirement.
func TestModuleRequiresNoController(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("go.mod")
	require.NoError(t, err)
	for _, forbidden := range []string{
		"github.com/openeverest/openeverest",
		"sigs.k8s.io/controller-runtime",
		"sigs.k8s.io/controller-tools",
		"k8s.io/client-go",
	} {
		assert.NotContains(t, string(data), forbidden)
	}
}
