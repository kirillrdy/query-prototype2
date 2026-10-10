package engine

import (
	"binaryquery/format"
	"context"
	"encoding/json"
	"go/ast"
	astparser "go/parser"
	gotoken "go/token"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// Run with BQUERY_ZIG_PARITY pointing to zig-out/bin/bquery-parity.
// Extracts the existing SQL corpus directly, so the port checks the actual
// documented Go semantics rather than independently chosen examples.
func TestZigEngineParity(t *testing.T) {
	binary := os.Getenv("BQUERY_ZIG_PARITY")
	if binary == "" {
		binary, _ = filepath.Abs("../../../zig-out/bin/bquery-parity")
		if _, err := os.Stat(binary); err != nil {
			t.Skip("build the differential runner with zig build parity")
		}
	}
	binary, _ = filepath.Abs(binary)
	fixtures := []format.Table{windowTables()["frames"], {Columns: []string{"path", "size", "pts_seconds", "is_keyframe"}, Rows: []format.Row{
		{"path": "a'b.mp4", "size": uint64(9007199254740993), "pts_seconds": 0.04, "is_keyframe": true},
		{"path": "other.mp4", "size": uint64(9007199254740992), "pts_seconds": 0.08, "is_keyframe": false},
	}}, {Columns: []string{"value", "n", "size"}, Rows: []format.Row{{"value": nil, "n": nil, "size": nil}, {"value": uint64(^uint64(0)), "n": uint64(9007199254740993), "size": uint64(^uint64(0))}, {"value": int64(-1), "n": 1.0, "size": 0}}}}
	files, _ := filepath.Glob("*_test.go")
	var sqls []string
	for _, file := range files {
		if strings.Contains(file, "zig_parity") {
			continue
		}
		parsed, err := astparser.ParseFile(gotoken.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(parsed, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != gotoken.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err == nil && strings.HasPrefix(strings.ToLower(strings.TrimSpace(s)), "select") {
				sqls = append(sqls, s)
			}
			return true
		})
	}
	checked := 0
	for _, table := range fixtures {
		for _, sql := range sqls {
			query, parseErr := Parse(sql)
			var expected Result
			err := parseErr
			if err == nil {
				expected, err = query.Execute(context.Background(), map[string]format.Table{"frames": table})
			}
			bytes, _ := json.Marshal(struct {
				SQL     string       `json:"sql"`
				Columns []string     `json:"columns"`
				Rows    []format.Row `json:"rows"`
			}{sql, table.Columns, table.Rows})
			path := filepath.Join(t.TempDir(), "input.json")
			if e := os.WriteFile(path, bytes, 0600); e != nil {
				t.Fatal(e)
			}
			output, nativeErr := exec.Command(binary, path).Output()
			if (err == nil) != (nativeErr == nil) {
				t.Fatalf("acceptance mismatch for %s\nGo: %v\nZig: %v %s", sql, err, nativeErr, output)
			}
			if err != nil {
				checked++
				continue
			}
			want, _ := json.Marshal(expected)
			var x, y any
			json.Unmarshal(want, &x)
			if e := json.Unmarshal(output, &y); e != nil {
				t.Fatalf("%s: %s", e, output)
			}
			if !reflect.DeepEqual(x, y) {
				t.Fatalf("result mismatch for %s\nGo: %s\nZig: %s", sql, want, output)
			}
			checked++
		}
	}
	t.Logf("compared %d native executions from %d SQL statements", checked, len(sqls))
}
