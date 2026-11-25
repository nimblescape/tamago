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

// offPath are the functions, and the types with all their methods, that
// the runtime never calls through a Provider, so they may write pointers
// with a write barrier: the constructors, the managers of the kernel with
// their pieces, the pieces of an owner, and package initialization.
var offPath = map[string]bool{
	"New":             true,
	"NewAddressSpace": true,
	"NewManager":      true,
	"NewOwner":        true,
	"NewProvider":     true,
	"NewTables":       true,
	"split":           true,
	"Manager":         true,
	"Buffer":          true,
	"Mapping":         true,
	"Region":          true,
	"Owner.SetDomain": true,
	"Owner.Hold":      true,
	"Owner.Drop":      true,
	"Owner.End":       true,
	"Owner.Pieces":    true,
	"Owner.NewChild":  true,
	"Owner.Release":   true,
	"Tables.Reach":    true,
	"Space.Prepared":  true,
	"remove":          true,
	"init":            true,
}

var writeBarrier = regexp.MustCompile(`^(.+\.go):(\d+):\d+: write barrier$`)

// TestNoWriteBarrier compiles this package and package amd64/paging with
// the compiler flag -d=wb, which reports every write barrier, and fails for
// each one outside offPath (see Provider). The build turns inlining off,
// so that the compiler reports each write barrier in the function that
// holds it. A write barrier in the code of another package, such as an
// instance of a generic function, cannot be traced to a function of these
// packages, so it fails too.
func TestNoWriteBarrier(t *testing.T) {
	gotool := filepath.Join(runtime.GOROOT(), "bin", "go")
	cmd := exec.Command(gotool, "build", "-gcflags=-d=wb -l", ".", "../../amd64/paging")
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

		if !strings.HasPrefix(m[1], "./") && !strings.HasPrefix(m[1], "../../amd64/paging/") {
			t.Errorf("%s: write barrier in the code of another package", m[1])
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

		if name := enclosing(fset, f, n); !offPath[name] && !offPath[strings.Split(name, ".")[0]] {
			t.Errorf("%s: write barrier in %s, which the runtime may call through a Provider", line[:len(line)-len(": write barrier")], name)
		}
	}
}

// enclosing returns the name of the function of f that holds the line n,
// with its receiver type for a method, or "init" for a line outside every
// function.
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

	return "init"
}
