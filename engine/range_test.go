package engine

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"testing"

	"binaryquery/format"
)

func paginationTables(lazy bool) map[string]format.Table {
	rows := make([]format.Row, 10)
	for i := range rows {
		rows[i] = format.Row{"n": i, "lower": 2, "upper": 8}
	}
	table := format.Table{Columns: []string{"n", "lower", "upper"}, Rows: rows}
	if lazy {
		table.Rows = nil
		table.Count = len(rows)
		table.ValueAt = func(i int, c string) any { return rows[i][c] }
	}
	return map[string]format.Table{"frames": table}
}

func TestOffsetBetweenBatchLazyAndStreaming(t *testing.T) {
	maxInt := strconv.Itoa(int(^uint(0) >> 1))
	cases := []struct {
		sql  string
		want []int
	}{
		{"SELECT n FROM frames OFFSET 3 LIMIT 2", []int{3, 4}},
		{"SELECT n FROM frames LIMIT 2 OFFSET 3", []int{3, 4}},
		{"SELECT n FROM frames ORDER BY n DESC LIMIT 2 OFFSET 3", []int{6, 5}},
		{"SELECT n FROM frames OFFSET 8", []int{8, 9}},
		{"SELECT n FROM frames LIMIT 0 OFFSET 4", []int{}},
		{"SELECT n FROM frames OFFSET 10 LIMIT 2", []int{}},
		{"SELECT n FROM frames OFFSET " + maxInt + " LIMIT " + maxInt, []int{}},
		{"SELECT n FROM frames LIMIT " + maxInt + " OFFSET 9", []int{9}},
		{"SELECT n FROM frames WHERE n BETWEEN 2 AND 4 LIMIT 10", []int{2, 3, 4}},
		{"SELECT n FROM frames WHERE n BETWEEN lower AND upper AND n >= 4 LIMIT 2 OFFSET 2", []int{6, 7}},
		{"SELECT n FROM frames WHERE n NOT BETWEEN 2 AND 8 LIMIT 10", []int{0, 1, 9}},
		{"SELECT n FROM frames WHERE n BETWEEN 4 AND 2 LIMIT 10", []int{}},
		{"SELECT n FROM (SELECT n FROM frames LIMIT 6 OFFSET 2) AS page WHERE n BETWEEN 4 AND 6 OFFSET 1 LIMIT 1", []int{5}},
	}
	for _, lazy := range []bool{false, true} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("lazy=%v/%s", lazy, tc.sql), func(t *testing.T) {
				q, err := Parse(tc.sql)
				if err != nil {
					t.Fatal(err)
				}
				result, err := q.Execute(context.Background(), paginationTables(lazy))
				if err != nil {
					t.Fatal(err)
				}
				got := []int{}
				for _, row := range result.Rows {
					got = append(got, row["n"].(int))
				}
				if !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("got %v want %v", got, tc.want)
				}
				streamed := Result{Rows: []format.Row{}}
				err = q.Stream(context.Background(), paginationTables(lazy), nil, func(chunk Result) error {
					if chunk.Rows == nil {
						streamed.Columns = chunk.Columns
					} else {
						streamed.Rows = append(streamed.Rows, chunk.Rows...)
					}
					return nil
				})
				if err != nil || !reflect.DeepEqual(streamed, result) {
					t.Fatalf("stream differs: %+v %+v %v", streamed, result, err)
				}
			})
		}
	}
}

func TestOffsetKeepsWindowInputAndSkipsScalarWork(t *testing.T) {
	for _, lazy := range []bool{false, true} {
		r := queryResult(t, "SELECT n, LAG(n) OVER (ORDER BY n DESC) AS previous FROM frames LIMIT 2 OFFSET 1", paginationTables(lazy), nil)
		if !reflect.DeepEqual(r.Rows, []format.Row{{"n": 1, "previous": 2}, {"n": 2, "previous": 3}}) {
			t.Fatal(r)
		}
		var evaluated []int
		functions := Functions{"expensive": {Arity: 1, Evaluate: func(ctx context.Context, args [][]any) ([]any, error) {
			out := make([]any, len(args))
			for i, a := range args {
				n := a[0].(int)
				evaluated = append(evaluated, n)
				out[i] = n
			}
			return out, nil
		}}}
		queryResult(t, "SELECT expensive(n) AS value FROM frames LIMIT 2 OFFSET 5", paginationTables(lazy), functions)
		if !reflect.DeepEqual(evaluated, []int{5, 6}) {
			t.Fatalf("evaluated skipped rows: %v", evaluated)
		}
		q, _ := Parse("SELECT n, LAG(n) OVER (ORDER BY n) AS previous FROM frames OFFSET 5")
		r, err := q.WithMaxRows(1).Execute(context.Background(), paginationTables(lazy))
		if err != nil || !reflect.DeepEqual(r.Rows, []format.Row{{"n": 5, "previous": 4}}) {
			t.Fatalf("capped page: %+v %v", r, err)
		}
	}
}

func TestBetweenNullsStringsAndExactNumbers(t *testing.T) {
	rows := []format.Row{
		{"n": 1, "lower": nil, "upper": 0},
		{"n": 1, "lower": nil, "upper": 2},
		{"n": 1, "lower": 2, "upper": nil},
		{"n": nil, "lower": 0, "upper": 2},
		{"n": 1, "lower": 1, "upper": 1},
	}
	for _, lazy := range []bool{false, true} {
		table := format.Table{Columns: []string{"n", "lower", "upper"}, Rows: rows}
		if lazy {
			table.Rows = nil
			table.Count = len(rows)
			table.ValueAt = func(i int, c string) any { return rows[i][c] }
		}
		for _, tc := range []struct {
			op    string
			count int
		}{{"BETWEEN", 1}, {"NOT BETWEEN", 2}} {
			r := queryResult(t, "SELECT n FROM frames WHERE n "+tc.op+" lower AND upper LIMIT 10", map[string]format.Table{"frames": table}, nil)
			if len(r.Rows) != tc.count {
				t.Fatalf("%s null semantics: %+v", tc.op, r)
			}
		}
	}
	tables := map[string]format.Table{"frames": {Columns: []string{"name", "size"}, Rows: []format.Row{
		{"name": "a", "size": uint64(9007199254740992)}, {"name": "b", "size": uint64(9007199254740993)}, {"name": "c", "size": uint64(9007199254740994)},
	}}}
	r := queryResult(t, "SELECT name FROM frames WHERE name BETWEEN 'a' AND 'c' AND size BETWEEN 9007199254740993 AND 9007199254740993", tables, nil)
	if !reflect.DeepEqual(r.Rows, []format.Row{{"name": "b"}}) {
		t.Fatal(r)
	}
}

func TestRangePaginationValidationAndBoundFunctions(t *testing.T) {
	for _, sql := range []string{
		"SELECT n FROM frames OFFSET", "SELECT n FROM frames OFFSET -1", "SELECT n FROM frames OFFSET 1.5", "SELECT n FROM frames OFFSET '2'",
		"SELECT n FROM frames OFFSET 18446744073709551616", "SELECT n FROM frames OFFSET 1 OFFSET 2", "SELECT n FROM frames LIMIT 1 LIMIT 2",
		"SELECT n FROM frames WHERE n BETWEEN 1", "SELECT n FROM frames WHERE n BETWEEN 1 AND", "SELECT n FROM frames WHERE n NOT 1 AND 2",
	} {
		if _, err := Parse(sql); err == nil {
			t.Fatalf("accepted %s", sql)
		}
	}
	for _, sql := range []string{"SELECT n FROM frames WHERE n BETWEEN lower AND missing", "SELECT n FROM frames WHERE n BETWEEN lower AND LAG(n) OVER ()"} {
		q, err := Parse(sql)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := q.Execute(context.Background(), paginationTables(false)); err == nil {
			t.Fatalf("accepted %s", sql)
		}
	}
	functions := Functions{"bound": {Arity: 1, Evaluate: func(ctx context.Context, args [][]any) ([]any, error) {
		out := make([]any, len(args))
		for i, tuple := range args {
			out[i] = tuple[0]
		}
		return out, nil
	}}}
	r := queryResult(t, "SELECT n FROM frames WHERE n BETWEEN bound(2) AND bound(4) OFFSET 1", paginationTables(true), functions)
	if !reflect.DeepEqual(r.Rows, []format.Row{{"n": 3}, {"n": 4}}) {
		t.Fatal(r)
	}
}

func TestLazyRangePaginationStopsAtPageEnd(t *testing.T) {
	reads := 0
	table := format.Table{Columns: []string{"n"}, Count: 1000000, ValueAt: func(i int, c string) any { reads++; return i }}
	r := queryResult(t, "SELECT n FROM frames WHERE n BETWEEN 2 AND 20 LIMIT 2 OFFSET 3", map[string]format.Table{"frames": table}, nil)
	if !reflect.DeepEqual(r.Rows, []format.Row{{"n": 5}, {"n": 6}}) || reads != 9 {
		t.Fatalf("page scan: %+v reads=%d", r, reads)
	}
}
