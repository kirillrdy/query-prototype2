package app

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"binaryquery/format"
	"binaryquery/video"
)

func request(s *Server, method, path string, data []byte) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://localhost"+path, bytes.NewReader(data))
	if method == "POST" {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
func emptyMovie(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "video.mp4")
	d := make([]byte, 8)
	binary.BigEndian.PutUint32(d, 8)
	copy(d[4:], "moov")
	if err := os.WriteFile(path, d, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func newTestServer(t *testing.T, paths ...string) *Server {
	t.Helper()
	s, err := New(context.Background(), paths)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestQueryProjectionAndErrors(t *testing.T) {
	s := newTestServer(t, emptyMovie(t))
	w := request(s, "POST", "/api/query", []byte(`{"sql":"SELECT path FROM files"}`))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var result struct {
		Columns []string
		Rows    []map[string]any
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Columns) != 1 || len(result.Rows) != 1 || result.Rows[0]["path"] != "video.mp4" || len(result.Rows[0]) != 1 {
		t.Fatalf("result: %+v", result)
	}
	for _, body := range []string{`{"sql":"SELECT wrong FROM frames"}`, `{"sql":"invalid"}`, `{"sql":"SELECT * FROM files","extra":true}`, `{"sql":"SELECT * FROM files"} {}`} {
		if w := request(s, "POST", "/api/query", []byte(body)); w.Code != 400 {
			t.Fatalf("accepted %s: %d", body, w.Code)
		}
	}
	if w := request(s, "GET", "/api/frame/unknown/raw", nil); w.Code != 404 {
		t.Fatal("unknown frame should return 404")
	}
	if w := request(newTestServer(t), "POST", "/api/query", []byte(`{"sql":"SELECT * FROM frames"}`)); w.Code != 400 {
		t.Fatal("query without video should fail")
	}
}

func TestDetectionFunctionsRegisteredWithoutDependencies(t *testing.T) {
	t.Setenv("BQUERY_DETECTION_MODEL", "")
	s := newTestServer(t, emptyMovie(t))
	for _, endpoint := range []string{"/api/query", "/api/query/stream"} {
		w := request(s, "POST", endpoint, []byte(`{"sql":"SELECT person_count(null) AS people, object_count(null, 'car') AS cars FROM files"}`))
		if w.Code != 200 {
			t.Fatalf("%s: %s", endpoint, w.Body.String())
		}
		if !bytes.Contains(w.Body.Bytes(), []byte(`"people":null`)) || !bytes.Contains(w.Body.Bytes(), []byte(`"cars":null`)) {
			t.Fatal(w.Body.String())
		}
	}
}

func TestUploadAndLocalOrigin(t *testing.T) {
	s := newTestServer(t)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	file, err := writer.CreateFormFile("file", "sample.mov")
	if err != nil {
		t.Fatal(err)
	}
	d, err := os.ReadFile(emptyMovie(t))
	if err != nil {
		t.Fatal(err)
	}
	file.Write(d)
	writer.Close()
	r := httptest.NewRequest("POST", "http://localhost/api/upload", &body)
	r.Header.Set("Content-Type", writer.FormDataContentType())
	r.Header.Set("Origin", "http://localhost")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := request(s, "GET", "/api/info", nil); !bytes.Contains(w.Body.Bytes(), []byte("sample.mov")) {
		t.Fatal(w.Body.String())
	}
	for _, url := range []string{"http://localhost/api/query", "http://attacker.invalid/api/query"} {
		r := httptest.NewRequest("POST", url, bytes.NewBufferString(`{"sql":"SELECT * FROM files"}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "http://attacker.invalid")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatal("cross-origin request accepted")
		}
	}
}

func TestRawBinaryDownloadAndSourceChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payload.mp4")
	data := []byte("0123456789")
	os.WriteFile(path, data, 0600)
	s := newTestServer(t)
	stat, _ := os.Stat(path)
	s.sources = []*source{{path: path, size: int64(len(data)), modified: stat.ModTime(), frames: map[string]format.Frame{"known": {Offset: 2, Size: 4}}}}
	w := request(s, "GET", "/api/frame/known/raw", nil)
	if w.Code != 200 || w.Body.String() != "2345" || w.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("raw: %d %s", w.Code, w.Body.String())
	}
	r := httptest.NewRequest("GET", "http://localhost/api/frame/known/raw", nil)
	r.Header.Set("Range", "bytes=1-2")
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 206 || w.Body.String() != "34" {
		t.Fatalf("range: %d %s", w.Code, w.Body.String())
	}
	os.WriteFile(path, []byte("changed source"), 0600)
	if w := request(s, "GET", "/api/frame/known/raw", nil); w.Code != 409 {
		t.Fatal("changed source must be rejected")
	}
}

func TestUncappedResultsAndNoDecoder(t *testing.T) {
	s := newTestServer(t, emptyMovie(t))
	table, err := s.sources[0].loadTable(context.Background(), "files")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1200; i++ {
		table.Rows = append(table.Rows, format.Row{"path": "sample"})
	}
	s.sources[0].tables["files"] = table
	w := request(s, "POST", "/api/query", []byte(`{"sql":"SELECT path FROM files"}`))
	var result struct {
		Rows      []format.Row
		Truncated bool
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != table.Len() || result.Truncated {
		t.Fatalf("rows: %d truncated=%v", len(result.Rows), result.Truncated)
	}
	s.renderer.decoder = &video.Decoder{}
	if err := s.renderer.prepare(context.Background(), s.sources[0], 1, nil); err == nil {
		t.Fatal("missing decoder must fail clearly")
	}
}

func TestStaticAssets(t *testing.T) {
	s := newTestServer(t)
	for _, path := range []string{"/", "/app.js", "/style.css"} {
		w := request(s, "GET", path, nil)
		if w.Code != 200 {
			t.Fatalf("%s: %d", path, w.Code)
		}
		if _, err := io.ReadAll(w.Result().Body); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRemoveSourcePreservesFileAndExcludesQueries(t *testing.T) {
	first, second := emptyMovie(t), emptyMovie(t)
	s := newTestServer(t, first, second)
	id := s.sources[0].id
	w := request(s, "DELETE", "/api/source/"+id, nil)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if _, err := os.Stat(first); err != nil {
		t.Fatal("removing a source must preserve the original file")
	}
	w = request(s, "POST", "/api/query", []byte(`{"sql":"SELECT path FROM files"}`))
	var result struct{ Rows []format.Row }
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 1 || len(s.sources) != 1 || s.sources[0].path != second {
		t.Fatal("removed source still participates in queries")
	}
	if w := request(s, "DELETE", "/api/source/"+id, nil); w.Code != 404 {
		t.Fatal("unknown source must return 404")
	}
}
