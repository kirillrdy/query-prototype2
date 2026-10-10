package video

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestDetectionDownloadCacheAndRepair(t *testing.T) {
	body := []byte("model contents")
	hash := sha256.Sum256(body)
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.Write(body) }))
	defer s.Close()
	dir := t.TempDir()
	asset := detectionAsset{"model.onnx", s.URL, hex.EncodeToString(hash[:]), 100}
	for i := 0; i < 2; i++ {
		path, err := downloadDetectionAsset(context.Background(), dir, asset)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(path)
		if string(got) != string(body) {
			t.Fatal(string(got))
		}
	}
	if calls.Load() != 1 {
		t.Fatal("cache was not reused")
	}
	if err := os.WriteFile(filepath.Join(dir, asset.name), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := downloadDetectionAsset(context.Background(), dir, asset); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatal("corrupt cache was not repaired")
	}
	asset.hash = "bad"
	asset.name = "bad.onnx"
	if _, err := downloadDetectionAsset(context.Background(), dir, asset); err == nil {
		t.Fatal("accepted wrong checksum")
	}
	asset.maxSize = 1
	if _, err := downloadDetectionAsset(context.Background(), dir, asset); err == nil {
		t.Fatal("accepted oversized download")
	}
	if _, err := os.Stat(filepath.Join(dir, asset.name)); !os.IsNotExist(err) {
		t.Fatal("failed download was published")
	}
	staging, _ := filepath.Glob(filepath.Join(dir, ".download-*"))
	if len(staging) != 0 {
		t.Fatalf("left staging files: %v", staging)
	}
}

func TestDetectionDownloadCancellation(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer s.Close()
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := downloadDetectionAsset(ctx, dir, detectionAsset{"model.onnx", s.URL, "", 100}); err == nil {
		t.Fatal("ignored cancellation")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatal("partial download remained")
	}
}

func TestDetectionLibraryExtractionAndRepair(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "runtime.tgz")
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	z := gzip.NewWriter(f)
	tarWriter := tar.NewWriter(z)
	for _, name := range []string{"../../escape", "runtime/lib/library.so"} {
		body := "native library"
		if err := tarWriter.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		io.WriteString(tarWriter, body)
	}
	tarWriter.Close()
	z.Close()
	f.Close()
	path, err := extractDetectionLibrary(context.Background(), archive, dir, "library.so")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(path) != dir {
		t.Fatal("escaped cache")
	}
	if err := os.WriteFile(path, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := extractDetectionLibrary(context.Background(), archive, dir, "library.so"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "native library" {
		t.Fatal("corrupt extracted library not repaired")
	}
}

func TestDetectionRuntimeTargets(t *testing.T) {
	for _, target := range [][2]string{{"linux", "amd64"}, {"linux", "arm64"}, {"darwin", "amd64"}, {"darwin", "arm64"}} {
		asset, lib, err := runtimeAsset(target[0], target[1])
		if err != nil || len(asset.hash) != 64 || lib == "" {
			t.Fatalf("%v %s %v", asset, lib, err)
		}
	}
	if _, _, err := runtimeAsset("windows", "amd64"); err == nil {
		t.Fatal("unsupported target accepted")
	}
}
