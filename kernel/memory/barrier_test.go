// Kernel memory management
// https://github.com/usbarmory/tamago
//
// Copyright (c) The TamaGo Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package memory

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// constructors may write pointers with a write barrier: the runtime never
// calls them through a Provider.
var constructors = map[string]bool{
	"New":         true,
	"NewOwner":    true,
	"NewProvider": true,
	"NewTables":   true,
	"split":       true,
}

var writeBarrier = regexp.MustCompile(`^(.+\.go):(\d+):\d+: write barrier$`)

// TestNoWriteBarrier compiles this package and package amd64/paging with
// the compiler flag -d=wb, which reports every write barrier, and fails for
// each one outside the constructors (see Provider).
func TestNoWriteBarrier(t *testing.T) {
	gotool := filepath.Join(runtime.GOROOT(), "bin", "go")
	cmd := exec.Command(gotool, "build", "-gcflags=-d=wb", ".", "../../amd64/paging")
	out, err := cmd.CombinedOutput()

	if err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	files := map[string]*ast.File{}
	fset := token.NewFileSet()

	for _, line := range strings.Split(string(out), "\n") {
		m := writeBarrier.FindStringSubmatch(line)

		if m == nil {
			continue
		}

		f, ok := files[m[1]]

		if !ok {
			if f, err = parser.ParseFile(fset, m[1], nil, 0); err != nil {
				t.Fatal(err)
			}

			files[m[1]] = f
		}

		n, _ := strconv.Atoi(m[2])

		if name := enclosing(fset, f, n); !constructors[name] {
			t.Errorf("%s: write barrier in %s, which the runtime may call through a Provider", line[:len(line)-len(": write barrier")], name)
		}
	}
}

// enclosing returns the name of the function of f that holds the line n,
// with its receiver type for a method.
func enclosing(fset *token.FileSet, f *ast.File, n int) string {
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)

		if !ok || fset.Position(fn.Pos()).Line > n || fset.Position(fn.End()).Line < n {
			continue
		}

		if fn.Recv == nil {
			return fn.Name.Name
		}

		recv := fn.Recv.List[0].Type

		if star, ok := recv.(*ast.StarExpr); ok {
			recv = star.X
		}

		if id, ok := recv.(*ast.Ident); ok {
			return id.Name + "." + fn.Name.Name
		}

		return fn.Name.Name
	}

	return "no function"
}
