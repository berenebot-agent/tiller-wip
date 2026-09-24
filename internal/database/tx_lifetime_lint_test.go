package database

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestNoTenantTxAcrossIOInSource is the static half of the tenant-transaction
// lifetime guard. A tenant transaction (store.Scope.RunTx) must never be held
// across provider network I/O or client streaming. The runtime guard in
// internal/server proves the current inference path; this lint catches a future
// callback that reaches for a network/IO call inside the transaction.
func TestNoTenantTxAcrossIOInSource(t *testing.T) {
	root := filepath.Join("..", "..")
	targets := []string{
		filepath.Join(root, "internal", "store"),
		filepath.Join(root, "internal", "server", "client.go"),
	}
	// Selector method names that are unambiguously network or streaming I/O.
	forbiddenMethods := map[string]bool{
		"RoundTrip":    true,
		"Flush":        true,
		"HydrateOAuth": true,
		"WriteString":  true,
		"ServeHTTP":    true,
		"Dial":         true,
		"NewRequest":   true,
	}
	forbiddenPackage := func(path string) bool {
		p := strings.Trim(path, `"`)
		return p == "net" || p == "net/http" || p == "net/url" || p == "io" || p == "bufio" || strings.HasPrefix(p, "golang.org/x/net")
	}

	fset := token.NewFileSet()
	var files []string
	for _, target := range targets {
		info, err := os.Stat(target)
		if err == nil && info.IsDir() {
			_ = filepath.WalkDir(target, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if !d.IsDir() && strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
					files = append(files, path)
				}
				return nil
			})
			continue
		}
		files = append(files, target)
	}
	sort.Strings(files)

	for _, path := range files {
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		aliases := map[string]bool{}
		for _, imp := range file.Imports {
			if !forbiddenPackage(imp.Path.Value) {
				continue
			}
			if imp.Name != nil {
				aliases[imp.Name.Name] = true
				continue
			}
			parts := strings.Split(strings.Trim(imp.Path.Value, `"`), "/")
			aliases[parts[len(parts)-1]] = true
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := ""
			switch fn := call.Fun.(type) {
			case *ast.SelectorExpr:
				name = fn.Sel.Name
			case *ast.Ident:
				name = fn.Name
			}
			if name != "RunTx" || len(call.Args) == 0 {
				return true
			}
			callback, ok := call.Args[len(call.Args)-1].(*ast.FuncLit)
			if !ok {
				return true
			}
			ast.Inspect(callback.Body, func(inner ast.Node) bool {
				sel, ok := inner.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if forbiddenMethods[sel.Sel.Name] {
					t.Errorf("%s: tenant transaction callback calls %s; no I/O may run inside RunTx", fset.Position(sel.Pos()), sel.Sel.Name)
					return true
				}
				if pkg, ok := sel.X.(*ast.Ident); ok && aliases[pkg.Name] {
					t.Errorf("%s: tenant transaction callback calls into package %s; no I/O may run inside RunTx", fset.Position(sel.Pos()), pkg.Name)
				}
				return true
			})
			return true
		})
	}
}
