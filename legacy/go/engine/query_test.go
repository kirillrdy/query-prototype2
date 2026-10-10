package engine

import (
	"context"
	"errors"
	"testing"

	"binaryquery/format"
)

func TestQuery(t *testing.T) {
	tables := map[string]format.Table{"frames": {Columns: []string{"path", "size", "pts_seconds", "is_keyframe"}, Rows: []format.Row{
		{"path": "a'b.mp4", "size": uint64(9007199254740993), "pts_seconds": 0.04, "is_keyframe": true},
		{"path": "other.mp4", "size": uint64(9007199254740992), "pts_seconds": 0.08, "is_keyframe": false},
	}}}
	cases := []struct {
		sql   string
		count int
	}{
		{"select path FROM frames WHERE path = 'a''b.mp4' AND size > 9007199254740992 LIMIT 1;", 1},
		{"SELECT * FROM frames WHERE pts_seconds >= 0.04 AND is_keyframe = true", 1},
		{"SELECT * FROM frames LIMIT 0", 0},
		{"SELECT * FROM frames WHERE size = '9007199254740993'", 0},
		{"SELECT * FROM frames WHERE path <> 'other.mp4'", 1},
	}
	for _, tc := range cases {
		q, err := Parse(tc.sql)
		if err != nil {
			t.Fatal(err)
		}
		r, err := q.Execute(context.Background(), tables)
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Rows) != tc.count {
			t.Fatalf("%s: %v", tc.sql, r)
		}
	}
}

func TestInvalidQueriesAndSchema(t *testing.T) {
	for _, sql := range []string{"", "SELECT FROM frames", "SELECT * FROM frames LIMIT -1", "SELECT * FROM frames WHERE size = )", "SELECT * FROM frames WHERE path = 'open", "SELECT * FROM frames ORDER BY", "SELECT path,path FROM frames", "SELECT * FROM frames; DROP TABLE frames"} {
		if _, err := Parse(sql); err == nil {
			t.Fatalf("accepted %q", sql)
		}
	}
	tables := map[string]format.Table{"frames": {Columns: []string{"size"}}}
	for _, sql := range []string{"SELECT bad FROM frames", "SELECT * FROM missing", "SELECT * FROM frames WHERE bad = 1"} {
		q, err := Parse(sql)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := q.Execute(context.Background(), tables); err == nil {
			t.Fatalf("accepted %q", sql)
		}
	}
	q, _ := Parse("SELECT * FROM frames WHERE size != 0")
	tables["frames"] = format.Table{Columns: []string{"size"}, Rows: []format.Row{{"size": nil}}}
	r, err := q.Execute(context.Background(), tables)
	if err != nil || len(r.Rows) != 0 {
		t.Fatalf("null comparison: %v %v", r, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := q.Execute(ctx, tables); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}
