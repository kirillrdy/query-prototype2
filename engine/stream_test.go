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

func TestUnlimitedOrderedFilterStreamsBeforeFinishingInference(t *testing.T) {
	q, _ := Parse("SELECT n, detect(n) AS people FROM frames WHERE detect(n) > 0 ORDER BY n")
	table := format.Table{Columns: []string{"n"}, Count: 6000, ValueAt: func(i int, _ string) any { return 5999 - i }}
	maxSeen := -1
	functions := Functions{"detect": {Arity: 1, MaxBatchSize: 32, Evaluate: func(ctx context.Context, args [][]any) ([]any, error) {
		out := make([]any, len(args))
		for i, tuple := range args {
			n := tuple[0].(int)
			if n >= 32 {
				t.Fatal("evaluated later batches before delivering the first match")
			}
			maxSeen = max(maxSeen, n)
			out[i] = 1
		}
		return out, nil
	}}}
	stop := errors.New("client stopped receiving")
	gotRow := false
	err := q.Stream(context.Background(), map[string]format.Table{"frames": table}, functions, func(chunk Result) error {
		if chunk.Rows == nil {
			return nil
		}
		gotRow = true
		if len(chunk.Rows) != 1 || chunk.Rows[0]["n"] != 0 {
			t.Fatalf("wrong first ordered row: %+v", chunk)
		}
		return stop
	})
	if !errors.Is(err, stop) || !gotRow || maxSeen != 31 {
		t.Fatalf("row=%v maxSeen=%d err=%v", gotRow, maxSeen, err)
	}
}

func TestFilteredStreamingMatchesBatchOrderingOffsetAndLimit(t *testing.T) {
	table := format.Table{Columns: []string{"n"}, Count: 120, ValueAt: func(i int, _ string) any { return 119 - i }}
	functions := Functions{"detect": {Arity: 1, MaxBatchSize: 32, Evaluate: func(ctx context.Context, args [][]any) ([]any, error) {
		out := make([]any, len(args))
		for i, tuple := range args {
			out[i] = tuple[0].(int) % 3
		}
		return out, nil
	}}}
	for _, sql := range []string{
		"SELECT n, detect(n) AS people FROM frames WHERE detect(n) > 0 ORDER BY n",
		"SELECT n, detect(n) AS people FROM frames WHERE n >= 3 AND detect(n) > 0 ORDER BY n DESC LIMIT 35 OFFSET 7",
		"SELECT n, detect(n) AS people FROM frames WHERE detect(n) > 0 LIMIT 1 OFFSET 65",
		"SELECT n FROM frames WHERE detect(n) > 0 LIMIT 0",
		"SELECT n FROM frames WHERE detect(n) > 0 ORDER BY n OFFSET 90",
		"SELECT n, detect(n) AS people FROM frames WHERE detect(n) > 0 ORDER BY people DESC, n LIMIT 5",
	} {
		q, err := Parse(sql)
		if err != nil {
			t.Fatal(err)
		}
		tables := map[string]format.Table{"frames": table}
		want, err := q.ExecuteWith(context.Background(), tables, functions)
		if err != nil {
			t.Fatal(err)
		}
		got := Result{Rows: []format.Row{}}
		err = q.Stream(context.Background(), tables, functions, func(chunk Result) error {
			if chunk.Rows == nil {
				got.Columns = chunk.Columns
			} else {
				got.Rows = append(got.Rows, chunk.Rows...)
			}
			return nil
		})
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: got=%+v want=%+v err=%v", sql, got, want, err)
		}
	}
}

func TestUsesFunctionIncludesPredicatesWindowsAndDerivedInputs(t *testing.T) {
	for _, sql := range []string{
		"SELECT person_count(frame) AS people FROM frames",
		"SELECT frame FROM frames WHERE person_count(frame) > 0",
		"SELECT frame FROM frames WHERE 0 < object_count(frame, 'car')",
		"SELECT LAG(person_count(frame)) OVER () AS previous FROM frames",
		"SELECT people FROM (SELECT person_count(frame) AS people FROM frames) AS detected",
	} {
		q, err := Parse(sql)
		if err != nil {
			t.Fatal(err)
		}
		if !q.UsesFunction("PERSON_COUNT", "object_count") {
			t.Fatal(sql)
		}
	}
	q, _ := Parse("SELECT frame FROM frames WHERE pts_seconds > 1")
	if q.UsesFunction("person_count", "object_count") {
		t.Fatal("plain query reported detection")
	}
}
