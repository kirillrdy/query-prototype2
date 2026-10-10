package engine

import (
	"context"
	"fmt"
	"testing"

	"binaryquery/format"
)

func TestEarlyPredicateBoundsFrameFunction(t *testing.T) {
	q, err := Parse("SELECT sample_index, person_count(frame) AS people FROM frames WHERE sample_index <= 100 AND person_count(frame) > 0 ORDER BY sample_index DESC LIMIT 3")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	functions := Functions{"person_count": {Arity: 1, Evaluate: func(_ context.Context, args [][]any) ([]any, error) {
		calls++
		if len(args) > 100 {
			return nil, fmt.Errorf("detection received %d frames instead of a bounded sample", len(args))
		}
		out := make([]any, len(args))
		for i, tuple := range args {
			index := tuple[0].(int64)
			if index > 100 {
				return nil, fmt.Errorf("detection received excluded frame %d", index)
			}
			out[i] = int64(0)
			if index%2 == 0 {
				out[i] = int64(1)
			}
		}
		return out, nil
	}}}
	table := format.Table{Columns: []string{"sample_index", "frame"}, Count: 5000, ValueAt: func(i int, _ string) any { return int64(i + 1) }}
	r, err := q.ExecuteWith(context.Background(), map[string]format.Table{"frames": table}, functions)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Rows) != 3 || calls == 0 {
		t.Fatalf("%+v calls=%d", r, calls)
	}
	for i, row := range r.Rows {
		if row["sample_index"] != int64(100-i*2) || row["people"] != int64(1) {
			t.Fatalf("%+v", r)
		}
	}
}

func TestEmptyEarlyPredicateSkipsFrameFunction(t *testing.T) {
	q, err := Parse("SELECT sample_index FROM frames WHERE sample_index < 1 AND person_count(frame) > 0")
	if err != nil {
		t.Fatal(err)
	}
	functions := Functions{"person_count": {Arity: 1, Evaluate: func(context.Context, [][]any) ([]any, error) {
		t.Fatal("detection called after all rows were excluded")
		return nil, nil
	}}}
	table := format.Table{Columns: []string{"sample_index", "frame"}, Rows: []format.Row{{"sample_index": int64(1), "frame": "unused"}, {"sample_index": nil, "frame": "unused"}}}
	r, err := q.ExecuteWith(context.Background(), map[string]format.Table{"frames": table}, functions)
	if err != nil || len(r.Rows) != 0 {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestOrderedDetectionLimitStopsAfterFirstMatchBeyond4096(t *testing.T) {
	q, err := Parse("SELECT frame, person_count(frame) AS people FROM frames WHERE person_count(frame) > 0 ORDER BY frame LIMIT 1")
	if err != nil {
		t.Fatal(err)
	}
	maxSeen := int64(0)
	functions := Functions{"person_count": {Arity: 1, MaxBatchSize: 32, Evaluate: func(_ context.Context, args [][]any) ([]any, error) {
		if len(args) > 32 {
			return nil, fmt.Errorf("oversized inference batch: %d", len(args))
		}
		out := make([]any, len(args))
		for i, tuple := range args {
			frame := tuple[0].(int64)
			maxSeen = max(maxSeen, frame)
			out[i] = int64(0)
			if frame >= 4501 {
				out[i] = int64(1)
			}
		}
		return out, nil
	}}}
	// Input order is reversed; sorting after an early LIMIT would be incorrect.
	table := format.Table{Columns: []string{"frame"}, Count: 6000, ValueAt: func(i int, _ string) any { return int64(6000 - i) }}
	r, err := q.ExecuteWith(context.Background(), map[string]format.Table{"frames": table}, functions)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Rows) != 1 || r.Rows[0]["frame"] != int64(4501) || r.Rows[0]["people"] != int64(1) {
		t.Fatalf("%+v", r)
	}
	if maxSeen > 4512 {
		t.Fatalf("scanned beyond the first matching batch: %d", maxSeen)
	}
}

func TestFullDetectionFilterBatchesAndComputedOrdering(t *testing.T) {
	q, err := Parse("SELECT frame, person_count(frame) AS people FROM frames WHERE person_count(frame) > 0 ORDER BY people DESC, frame DESC LIMIT 2 OFFSET 1")
	if err != nil {
		t.Fatal(err)
	}
	scanned := 0
	functions := Functions{"person_count": {Arity: 1, MaxBatchSize: 32, Evaluate: func(_ context.Context, args [][]any) ([]any, error) {
		if len(args) > 32 {
			return nil, fmt.Errorf("oversized inference batch: %d", len(args))
		}
		out := make([]any, len(args))
		scanned += len(args)
		for i, tuple := range args {
			out[i] = tuple[0]
		}
		return out, nil
	}}}
	table := format.Table{Columns: []string{"frame"}, Count: 5000, ValueAt: func(i int, _ string) any { return int64(i + 1) }}
	r, err := q.ExecuteWith(context.Background(), map[string]format.Table{"frames": table}, functions)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Rows) != 2 || r.Rows[0]["frame"] != int64(4999) || r.Rows[1]["frame"] != int64(4998) {
		t.Fatalf("%+v", r)
	}
	if scanned < 5000 {
		t.Fatalf("computed ordering skipped required rows: %d", scanned)
	}
}
