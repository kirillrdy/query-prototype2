package app

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLargeLocalFileRegistrationAndDeferredParsing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.mp4")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	// A sparse 5 GiB media box exercises 64-bit offsets without allocating or
	// copying its payload. The movie header is at the end of the file.
	const mediaSize = int64(5 << 30)
	header := make([]byte, 16)
	binary.BigEndian.PutUint32(header, 1)
	copy(header[4:], "mdat")
	binary.BigEndian.PutUint64(header[8:], uint64(mediaSize))
	if _, err := f.WriteAt(header, 0); err != nil {
		t.Fatal(err)
	}
	moov := make([]byte, 8)
	binary.BigEndian.PutUint32(moov, 8)
	copy(moov[4:], "moov")
	if _, err := f.WriteAt(moov, mediaSize); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t)
	body, _ := json.Marshal(map[string]string{"path": path})
	if w := request(s, "POST", "/api/source", body); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	src := s.sources[0]
	if len(src.tables) != 0 || len(src.frames) != 0 {
		t.Fatal("registration indexed the video")
	}
	w := request(s, "GET", "/api/info", nil)
	var info struct {
		Sources []struct{ Frames *int }
		Schemas map[string][]string
	}
	if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.Sources[0].Frames != nil || len(info.Schemas["frames"]) == 0 || len(src.tables) != 0 {
		t.Fatal("info should expose schema without indexing")
	}
	w = request(s, "POST", "/api/query", []byte(`{"sql":"SELECT size FROM files"}`))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if _, ok := src.tables["frames"]; ok {
		t.Fatal("file query indexed frames")
	}
	if src.path != path {
		t.Fatal("local path should be read in place")
	}
}

func TestInvalidMetadataFailsAtQueryTimeAndCanceledIndexCanRetry(t *testing.T) {
	path := emptyMovie(t)
	// Probe accepts the type; the invalid size is validated by the first query.
	data := make([]byte, 8)
	binary.BigEndian.PutUint32(data, 100)
	copy(data[4:], "moov")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, path)
	if len(s.sources[0].tables) != 0 {
		t.Fatal("registration parsed malformed metadata")
	}
	if w := request(s, "POST", "/api/query", []byte(`{"sql":"SELECT * FROM files"}`)); w.Code != 400 {
		t.Fatal("query accepted invalid metadata")
	}
	if len(s.sources[0].tables) != 0 {
		t.Fatal("failed query published partial index")
	}
	s = newTestServer(t, emptyMovie(t))
	src := s.sources[0]
	src.indexGate <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := src.loadTable(ctx, "files"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled waiter: %v", err)
	}
	<-src.indexGate
	if _, err := src.loadTable(context.Background(), "files"); err != nil {
		t.Fatal(err)
	}
}
