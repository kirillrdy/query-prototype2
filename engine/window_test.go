package engine

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"binaryquery/format"
)

func windowTables() map[string]format.Table {
	return map[string]format.Table{"frames": {Columns: []string{"path", "track_id", "sample_index", "pts_seconds", "frame", "value", "offset"}, Rows: []format.Row{
		{"path": "a", "track_id": 1, "sample_index": 1, "pts_seconds": 0.0, "frame": "a1", "value": 10, "offset": 1},
		{"path": "a", "track_id": 1, "sample_index": 2, "pts_seconds": 0.3, "frame": "a4", "value": 40, "offset": 2},
		{"path": "a", "track_id": 1, "sample_index": 3, "pts_seconds": 0.1, "frame": "a2", "value": nil, "offset": 0},
		{"path": "b", "track_id": 1, "sample_index": 1, "pts_seconds": 0.0, "frame": "b1", "value": 100, "offset": 1},
		{"path": "a", "track_id": 2, "sample_index": 1, "pts_seconds": 0.0, "frame": "other", "value": 1000, "offset": 1},
		{"path": "a", "track_id": 1, "sample_index": 4, "pts_seconds": 0.2, "frame": "a3", "value": 30, "offset": nil},
	}}}
}
func queryResult(t *testing.T, sql string, tables map[string]format.Table, functions Functions) Result {
	t.Helper()
	q, err := Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	r, err := q.ExecuteWith(context.Background(), tables, functions)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestLagPartitionsAndPresentationOrder(t *testing.T) {
	r := queryResult(t, `SELECT frame, LAG(frame) OVER (PARTITION BY path, track_id ORDER BY pts_seconds, sample_index) AS previous_frame FROM frames`, windowTables(), nil)
	want := []any{nil, "a3", "a1", nil, nil, "a2"}
	for i, row := range r.Rows {
		if row["previous_frame"] != want[i] {
			t.Fatalf("row %d: %v", i, row)
		}
	}
}

func TestLagOffsetDefaultRespectNulls(t *testing.T) {
	r := queryResult(t, `SELECT frame, LAG(value, 2, -1) OVER (PARTITION BY path, track_id ORDER BY pts_seconds) AS prior FROM frames WHERE path = 'a' AND track_id = 1 ORDER BY pts_seconds`, windowTables(), nil)
	want := []any{int64(-1), int64(-1), 10, nil}
	for i, row := range r.Rows {
		if row["prior"] != want[i] {
			t.Fatalf("row %d: %v", i, row)
		}
	}
	r = queryResult(t, `SELECT frame, LAG(value, offset, -1) OVER (PARTITION BY path, track_id ORDER BY pts_seconds) AS prior FROM frames WHERE path = 'a' AND track_id = 1 ORDER BY pts_seconds`, windowTables(), nil)
	want = []any{int64(-1), nil, nil, nil}
	for i, row := range r.Rows {
		if row["prior"] != want[i] {
			t.Fatalf("variable offset row %d: %v", i, row)
		}
	}
	r = queryResult(t, `SELECT LAG(frame, 0) OVER () AS same, frame FROM frames LIMIT 1`, windowTables(), nil)
	if r.Rows[0]["same"] != r.Rows[0]["frame"] {
		t.Fatal("zero offset should return current row")
	}
}

func TestWhereBeforeLagAndLimitAfterWindow(t *testing.T) {
	r := queryResult(t, `SELECT frame, LAG(frame) OVER (PARTITION BY path, track_id ORDER BY pts_seconds) AS previous_frame FROM frames WHERE pts_seconds >= 0.1 AND path = 'a' AND track_id = 1 ORDER BY pts_seconds DESC LIMIT 1`, windowTables(), nil)
	if len(r.Rows) != 1 || r.Rows[0]["frame"] != "a4" || r.Rows[0]["previous_frame"] != "a3" {
		t.Fatal(r)
	}
	r = queryResult(t, `SELECT frame, previous_frame FROM (SELECT frame, pts_seconds, LAG(frame) OVER (PARTITION BY path, track_id ORDER BY pts_seconds) AS previous_frame FROM frames WHERE path = 'a' AND track_id = 1) AS sequenced WHERE pts_seconds >= 0.1 ORDER BY pts_seconds LIMIT 1`, windowTables(), nil)
	if r.Rows[0]["frame"] != "a2" || r.Rows[0]["previous_frame"] != "a1" {
		t.Fatal(r)
	}
}

func TestNestedFunctionBatchAndSubqueryAlias(t *testing.T) {
	var calls int
	var tuples [][]any
	functions := Functions{"motion": {Arity: 2, Evaluate: func(ctx context.Context, args [][]any) ([]any, error) {
		calls++
		tuples = args
		out := make([]any, len(args))
		for i, a := range args {
			if a[1] != nil {
				out[i] = fmt.Sprint(a[0], ":", a[1])
			}
		}
		return out, nil
	}}}
	r := queryResult(t, `SELECT motion(frame, LAG(frame) OVER (PARTITION BY path, track_id ORDER BY pts_seconds)) AS score FROM frames WHERE path = 'a' AND track_id = 1 ORDER BY pts_seconds LIMIT 2`, windowTables(), functions)
	if calls != 1 || len(tuples) != 2 || r.Rows[0]["score"] != nil || r.Rows[1]["score"] != "a2:a1" {
		t.Fatalf("%v calls=%d tuples=%v", r, calls, tuples)
	}
	r = queryResult(t, `SELECT motion(frame, previous_frame) AS score FROM (SELECT frame, LAG(frame) OVER (PARTITION BY path, track_id ORDER BY pts_seconds) AS previous_frame FROM frames) AS sequenced LIMIT 2`, windowTables(), functions)
	if r.Rows[1]["score"] != "a4:a3" {
		t.Fatal(r)
	}
}

func TestOrderAliasesNullsAndOuterFilter(t *testing.T) {
	r := queryResult(t, `SELECT value AS v FROM frames ORDER BY v DESC NULLS LAST LIMIT 3`, windowTables(), nil)
	want := []any{1000, 100, 40}
	for i, row := range r.Rows {
		if row["v"] != want[i] {
			t.Fatal(r)
		}
	}
	r = queryResult(t, `SELECT frame FROM (SELECT frame, LAG(frame) OVER (PARTITION BY path, track_id ORDER BY pts_seconds) AS previous_frame FROM frames) AS s WHERE previous_frame IS NULL`, windowTables(), nil)
	if len(r.Rows) != 3 {
		t.Fatal(r)
	}
}

func TestWindowValidationAndFunctionErrors(t *testing.T) {
	for _, sql := range []string{
		`SELECT LAG(frame) FROM frames`, `SELECT LAG() OVER () FROM frames`, `SELECT LAG(frame) OVER (ORDER BY) FROM frames`,
		`SELECT frame AS f, value AS f FROM frames`, `SELECT * FROM (SELECT frame FROM frames)`,
	} {
		if _, err := Parse(sql); err == nil {
			t.Fatalf("accepted %q", sql)
		}
	}
	for _, sql := range []string{
		`SELECT LAG(frame) OVER (PARTITION BY missing) FROM frames`, `SELECT LAG(frame) OVER (ORDER BY missing) FROM frames`,
		`SELECT frame FROM frames WHERE LAG(frame) OVER () IS NULL`, `SELECT LAG(LAG(frame) OVER ()) OVER () FROM frames`,
		`SELECT LAG(frame, -1) OVER () FROM frames`, `SELECT LAG(frame, 0.5) OVER () FROM frames`,
		`SELECT missing(frame) FROM frames`, `SELECT * FROM frames ORDER BY missing`,
		`SELECT frame AS previous_frame, LAG(previous_frame) OVER () AS prior FROM frames`,
	} {
		q, err := Parse(sql)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := q.Execute(context.Background(), windowTables()); err == nil {
			t.Fatalf("accepted %q", sql)
		}
	}
	q, _ := Parse(`SELECT custom(frame) FROM frames`)
	_, err := q.ExecuteWith(context.Background(), windowTables(), Functions{"custom": {Arity: 1, Evaluate: func(context.Context, [][]any) ([]any, error) { return nil, context.Canceled }}})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	_, err = q.ExecuteWith(context.Background(), windowTables(), Functions{"custom": {Arity: 1, Evaluate: func(context.Context, [][]any) ([]any, error) { return []any{}, nil }}})
	if err == nil {
		t.Fatal("wrong batch result size accepted")
	}
}

func TestWithMaxRowsPreservesLagInput(t *testing.T) {
	q, err := Parse(`SELECT frame, LAG(frame) OVER (PARTITION BY path, track_id ORDER BY pts_seconds) AS previous_frame FROM frames WHERE path = 'a' AND track_id = 1 ORDER BY pts_seconds DESC`)
	if err != nil {
		t.Fatal(err)
	}
	r, err := q.WithMaxRows(1).Execute(context.Background(), windowTables())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.Rows, []format.Row{{"frame": "a4", "previous_frame": "a3"}}) {
		t.Fatal(r)
	}
}
