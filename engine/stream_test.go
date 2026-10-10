package engine

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"binaryquery/format"
)

func TestStreamMatchesBatchWithWindowsOrderingAndSubqueries(t *testing.T) {
	rows := []format.Row{}
	for i := 0; i < 80; i++ {
		rows = append(rows, format.Row{"n": i, "group": i % 3})
	}
	tables := map[string]format.Table{"frames": {Columns: []string{"n", "group"}, Rows: rows}}
	functions := Functions{"delta": {Arity: 2, Evaluate: func(ctx context.Context, args [][]any) ([]any, error) {
		out := make([]any, len(args))
		for i, tuple := range args {
			if tuple[1] != nil {
				out[i] = tuple[0].(int) - tuple[1].(int)
			}
		}
		return out, nil
	}}}
	for _, sql := range []string{
		"SELECT n, delta(n, LAG(n) OVER (PARTITION BY group ORDER BY n DESC)) AS change FROM frames ORDER BY group, n LIMIT 73",
		"SELECT n, LAG(n, 2, -1) OVER (ORDER BY n) AS previous FROM frames WHERE n >= 2 ORDER BY n DESC LIMIT 41",
		"SELECT n, previous FROM (SELECT n, LAG(n) OVER (ORDER BY n) AS previous FROM frames) AS input WHERE n >= 4 LIMIT 33",
		"SELECT n FROM frames LIMIT 0",
	} {
		q, err := Parse(sql)
		if err != nil {
			t.Fatal(err)
		}
		want, err := q.ExecuteWith(context.Background(), tables, functions)
		if err != nil {
			t.Fatal(err)
		}
		got := Result{Rows: []format.Row{}}
		schemaSeen, batches := false, 0
		err = q.Stream(context.Background(), tables, functions, func(chunk Result) error {
			if chunk.Rows == nil {
				schemaSeen = true
				got.Columns = chunk.Columns
				return nil
			}
			if !schemaSeen {
				t.Fatal("rows arrived before schema")
			}
			if batches == 0 && len(chunk.Rows) != 1 {
				t.Fatal("first batch should contain one completed row")
			}
			batches++
			got.Rows = append(got.Rows, chunk.Rows...)
			return nil
		})
		if err != nil || !schemaSeen || !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: stream %+v differs from batch %+v, err=%v", sql, got, want, err)
		}
	}
}

func TestStreamBackpressureStopsExpensiveWork(t *testing.T) {
	q, _ := Parse("SELECT slow(n) AS value FROM frames")
	tables := map[string]format.Table{"frames": {Columns: []string{"n"}, Rows: []format.Row{{"n": 0}, {"n": 1}}}}
	evaluated := 0
	functions := Functions{"slow": {Arity: 1, Evaluate: func(ctx context.Context, args [][]any) ([]any, error) {
		evaluated += len(args)
		return []any{args[0][0]}, nil
	}}}
	stop := errors.New("client disconnected")
	err := q.Stream(context.Background(), tables, functions, func(chunk Result) error {
		if chunk.Rows != nil {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) || evaluated != 1 {
		t.Fatalf("continued after callback stopped: evaluated=%d err=%v", evaluated, err)
	}
}
