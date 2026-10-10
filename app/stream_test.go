package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"binaryquery/engine"
	"binaryquery/format"
)

type streamEvent struct {
	Type      string       `json:"type"`
	Columns   []string     `json:"columns"`
	Rows      []format.Row `json:"rows"`
	Error     string       `json:"error"`
	Truncated bool         `json:"truncated"`
}

func TestStreamFlushesFirstRowBeforeRemainingWorkAndCancels(t *testing.T) {
	s := newTestServer(t)
	q, _ := engine.Parse("SELECT slow(n) AS value FROM frames")
	tables := map[string]format.Table{"frames": {Columns: []string{"n"}, Rows: []format.Row{{"n": 0}, {"n": 1}}}}
	canceled := make(chan struct{})
	functions := engine.Functions{"slow": {Arity: 1, Evaluate: func(ctx context.Context, args [][]any) ([]any, error) {
		if args[0][0] == 0 {
			return []any{0}, nil
		}
		<-ctx.Done()
		close(canceled)
		return nil, ctx.Err()
	}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.streamQuery(r.Context(), w, q, tables, functions) }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, "POST", server.URL, nil)
	response, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	decoder := json.NewDecoder(response.Body)
	var schema, first streamEvent
	if err := decoder.Decode(&schema); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&first); err != nil {
		t.Fatal(err)
	}
	if schema.Type != "schema" || first.Type != "rows" || len(first.Rows) != 1 || first.Rows[0]["value"] != float64(0) {
		t.Fatalf("first events: %+v %+v", schema, first)
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("disconnect did not cancel function")
	}
}

func TestStreamProtocolUncappedAndPartialErrors(t *testing.T) {
	s := newTestServer(t, emptyMovie(t))
	// Exercise the actual route beyond the former 500-row cap.
	table, err := s.sources[0].loadTable(context.Background(), "files")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1200; i++ {
		table.Rows = append(table.Rows, format.Row{"path": "sample"})
	}
	s.sources[0].tables["files"] = table
	w := request(s, "POST", "/api/query/stream", []byte(`{"sql":"SELECT path FROM files"}`))
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("response: %d %s", w.Code, w.Body.String())
	}
	decoder := json.NewDecoder(w.Body)
	count := 0
	for {
		var event streamEvent
		if err := decoder.Decode(&event); err != nil {
			t.Fatal(err)
		}
		count += len(event.Rows)
		if event.Type == "done" {
			if count != table.Len() || event.Truncated {
				t.Fatalf("count=%d truncated=%v", count, event.Truncated)
			}
			break
		}
	}
	q, _ := engine.Parse("SELECT fail(n) AS value FROM frames")
	tables := map[string]format.Table{"frames": {Columns: []string{"n"}, Rows: []format.Row{{"n": 0}, {"n": 1}}}}
	functions := engine.Functions{"fail": {Arity: 1, Evaluate: func(ctx context.Context, args [][]any) ([]any, error) {
		if args[0][0] == 0 {
			return []any{0}, nil
		}
		return nil, fmt.Errorf("decoder stopped")
	}}}
	w = httptest.NewRecorder()
	s.streamQuery(context.Background(), w, q, tables, functions)
	if !bytes.Contains(w.Body.Bytes(), []byte(`"type":"error"`)) || bytes.Contains(w.Body.Bytes(), []byte(`"type":"done"`)) {
		t.Fatal(w.Body.String())
	}
	if w := request(s, "POST", "/api/query/stream", []byte(`{"sql":"SELECT invalid FROM files"}`)); w.Code != 400 {
		t.Fatal("invalid query should fail before streaming")
	}
}

func TestOrderedDetectionFilterFlushesMatchesAndCancels(t *testing.T) {
	s := newTestServer(t)
	q, _ := engine.Parse("SELECT n, detect(n) AS people FROM frames WHERE detect(n) > 0 ORDER BY n")
	tables := map[string]format.Table{"frames": {Columns: []string{"n"}, Count: 6000, ValueAt: func(i int, _ string) any { return i }}}
	canceled := make(chan struct{})
	functions := engine.Functions{"detect": {Arity: 1, MaxBatchSize: 32, Evaluate: func(ctx context.Context, args [][]any) ([]any, error) {
		if args[0][0].(int) >= 32 {
			<-ctx.Done()
			close(canceled)
			return nil, ctx.Err()
		}
		out := make([]any, len(args))
		for i := range out {
			out[i] = 1
		}
		return out, nil
	}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.streamQuery(r.Context(), w, q, tables, functions) }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, "POST", server.URL, nil)
	response, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	decoder := json.NewDecoder(response.Body)
	var schema, first streamEvent
	if err := decoder.Decode(&schema); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&first); err != nil {
		t.Fatal(err)
	}
	if schema.Type != "schema" || first.Type != "rows" || len(first.Rows) != 1 || first.Rows[0]["n"] != float64(0) {
		t.Fatalf("%+v %+v", schema, first)
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("long detection scan ignored disconnect")
	}
}
