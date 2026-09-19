package database

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestTenantSQLStaysInStore enforces AGENTS.md invariant 14: internal/store is
// the single boundary for tenant-table SQL. It scans every non-test Go source
// file outside internal/store and internal/database for a tenant table name
// immediately following an SQL verb, which catches an unscoped query sneaking
// back into a handler, provider or auth path.
func TestTenantSQLStaysInStore(t *testing.T) {
	root := filepath.Join("..", "..")
	verbs := `(?:from|into|join|update)`
	patterns := make([]*regexp.Regexp, 0, len(TenantTables()))
	for _, table := range TenantTables() {
		patterns = append(patterns, regexp.MustCompile(`(?i)\b`+verbs+`\s+`+regexp.QuoteMeta(table)+`\b`))
	}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == ".git" || rel == "data" || rel == "node_modules" || rel == "vendor" || rel == "tests" {
				return filepath.SkipDir
			}
			if rel == "internal/store" || rel == "internal/database" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			value, uerr := strconv.Unquote(lit.Value)
			if uerr != nil {
				return true
			}
			for _, re := range patterns {
				if re.MatchString(value) {
					t.Errorf("%s: tenant-table SQL must live in internal/store: %q", fset.Position(lit.Pos()), value)
					break
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
