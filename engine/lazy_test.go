package engine

import (
	"context"
	"testing"

	"binaryquery/format"
)

func TestLazyScanStopsAtLimitAndWindowsKeepFullInput(t *testing.T) {
	reads := 0
	table := format.Table{Columns: []string{"n"}, Count: 1000000, ValueAt: func(i int, c string) any { reads++; return i }}
	q, err := Parse("SELECT n FROM frames WHERE n >= 2 LIMIT 3")
	if err != nil {
		t.Fatal(err)
	}
	r, err := q.Execute(context.Background(), map[string]format.Table{"frames": table})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Rows) != 3 || r.Rows[0]["n"] != 2 || r.Rows[2]["n"] != 4 || reads != 8 {
		t.Fatalf("scan: %+v, reads %d", r, reads)
	}
	table.Count = 10
	q, _ = Parse("SELECT LAG(n) OVER (ORDER BY n DESC) AS previous FROM frames LIMIT 1")
	r, err = q.Execute(context.Background(), map[string]format.Table{"frames": table})
	if err != nil || r.Rows[0]["previous"] != 1 {
		t.Fatalf("window lost complete input: %+v %v", r, err)
	}
}
