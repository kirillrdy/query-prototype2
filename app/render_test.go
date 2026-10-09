package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// A generated moving pattern makes selecting the wrong B-frame observable.
// Compare sparse query results with a separate full-stream image decode.
func TestRenderedSelectionMatchesPicturesAndRawBytes(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dir := t.TempDir()
	path := filepath.Join(dir, "reordered.mp4")
	if _, err := command(ctx, ffmpeg, "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=160x96:rate=6", "-t", "2", "-c:v", "mpeg4", "-bf", "2", path); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, path)
	w := request(s, "POST", "/api/query", []byte(`{"sql":"SELECT frame, path FROM frames WHERE sample_index >= 2 AND sample_index <= 5"}`))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var result struct {
		Rows []struct {
			Frame FrameValue `json:"frame"`
			Path  string     `json:"path"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 4 {
		t.Fatal("expected four selected frames")
	}
	ids := make([]string, len(result.Rows))
	for i, row := range result.Rows {
		ids[i] = row.Frame.ID
	}
	body, _ := json.Marshal(map[string]any{"ids": ids})
	w = request(s, "POST", "/api/render", body)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	baseline := filepath.Join(dir, "baseline")
	if err := os.Mkdir(baseline, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := command(ctx, ffmpeg, "-v", "error", "-nostdin", "-ignore_editlist", "1", "-i", path, "-an", "-vf", "scale=w='min(1280,iw)':h='min(720,ih)':force_original_aspect_ratio=decrease", "-fps_mode", "vfr", "-q:v", "2", filepath.Join(baseline, "%06d.jpg")); err != nil {
		t.Fatal(err)
	}
	var ordered []FrameValue
	for _, r := range s.sources[0].tables["frames"].Rows {
		ordered = append(ordered, r["frame"].(FrameValue))
	}
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].PTS < ordered[j].PTS })
	ordinals := map[string]int{}
	for i, f := range ordered {
		ordinals[f.ID] = i + 1
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range result.Rows {
		f := row.Frame
		w := request(s, "GET", f.RawURL, nil)
		if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), data[f.Offset:f.Offset+int64(f.Size)]) {
			t.Fatalf("sample %d raw bytes mismatch", f.SampleIndex)
		}
		w = request(s, "GET", f.PreviewURL, nil)
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
		expected, err := os.ReadFile(filepath.Join(baseline, fmt.Sprintf("%06d.jpg", ordinals[f.ID])))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(w.Body.Bytes(), expected) {
			t.Fatalf("sample %d rendered the wrong picture", f.SampleIndex)
		}
	}
	// The same selection remains available from the preview cache.
	w = request(s, "POST", "/api/render", body)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
}
