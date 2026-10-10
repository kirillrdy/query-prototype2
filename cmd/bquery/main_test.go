package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"binaryquery/engine"
)

func TestMultiFileLimitAndAtomicOutput(t *testing.T) {
	var paths []string
	for _, name := range []string{"first.mp4", "second.mov"} {
		path := filepath.Join(t.TempDir(), name)
		d := make([]byte, 8)
		binary.BigEndian.PutUint32(d, 8)
		copy(d[4:], "moov")
		if err := os.WriteFile(path, d, 0600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	var out, stderr bytes.Buffer
	args := append([]string{"-query", "SELECT path FROM files LIMIT 1"}, paths...)
	if err := run(context.Background(), args, &out, &stderr); err != nil {
		t.Fatal(err)
	}
	var result engine.Result
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 1 || result.Rows[0]["path"] != paths[0] {
		t.Fatalf("global limit: %v", result)
	}
	out.Reset()
	paged := append([]string{"-query", "SELECT path FROM files LIMIT 1 OFFSET 1"}, paths...)
	if err := run(context.Background(), paged, &out, &stderr); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 1 || result.Rows[0]["path"] != paths[1] {
		t.Fatalf("global offset: %+v", result)
	}
	out.Reset()
	args = append(args, filepath.Join(t.TempDir(), "missing.mp4"))
	if err := run(context.Background(), args, &out, &stderr); err == nil {
		t.Fatal("expected missing input error")
	}
	if out.Len() != 0 {
		t.Fatal("emitted partial result on input failure")
	}
}
