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

func command(ctx context.Context, program string, args ...string) ([]byte, error) {
	data, err := exec.CommandContext(ctx, program, args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%v: %s", err, data)
	}
	return data, nil
}

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
	w := request(s, "POST", "/api/query", []byte(`{"sql":"SELECT frame, path FROM frames WHERE sample_index BETWEEN 2 AND 5"}`))
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
	indexed := s.sources[0].tables["frames"]
	for i := 0; i < indexed.Len(); i++ {
		ordered = append(ordered, indexed.Value(i, "frame").(FrameValue))
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
	// Verify a motion/LAG query against a separate full grayscale decode.
	grayDir := filepath.Join(dir, "gray-baseline")
	if err := os.Mkdir(grayDir, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := command(ctx, ffmpeg, "-v", "error", "-nostdin", "-ignore_editlist", "1", "-i", path, "-an", "-vf", "scale=160:90,format=gray", "-fps_mode", "vfr", "-c:v", "rawvideo", "-pix_fmt", "gray", "-f", "image2", filepath.Join(grayDir, "%06d.gray")); err != nil {
		t.Fatal(err)
	}
	w = request(s, "POST", "/api/query", []byte(`{"sql":"SELECT frame, motion(frame, LAG(frame) OVER (PARTITION BY path, track_id ORDER BY pts_seconds, sample_index)) AS motion_score FROM frames WHERE sample_index BETWEEN 2 AND 5 ORDER BY pts_seconds"}`))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var motionResult struct {
		Rows []struct {
			Frame FrameValue `json:"frame"`
			Score *float64   `json:"motion_score"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &motionResult); err != nil {
		t.Fatal(err)
	}
	if len(motionResult.Rows) != 4 || motionResult.Rows[0].Score != nil {
		t.Fatalf("motion rows: %+v", motionResult)
	}
	var previous []byte
	for i, row := range motionResult.Rows {
		current, err := os.ReadFile(filepath.Join(grayDir, fmt.Sprintf("%06d.gray", ordinals[row.Frame.ID])))
		if err != nil {
			t.Fatal(err)
		}
		if i > 0 {
			var delta int
			for j, pixel := range current {
				d := int(pixel) - int(previous[j])
				if d < 0 {
					d = -d
				}
				delta += d
			}
			want := float64(delta) / (255 * float64(len(current)))
			if row.Score == nil || *row.Score != want || want <= 0 {
				t.Fatalf("sample %d: score=%v want=%g", row.Frame.SampleIndex, row.Score, want)
			}
		}
		previous = current
	}
	cold := newTestServer(t, path)
	streamed := request(cold, "POST", "/api/query/stream", []byte(`{"sql":"SELECT frame, motion(frame, LAG(frame) OVER (PARTITION BY path, track_id ORDER BY pts_seconds, sample_index)) AS motion_score FROM frames WHERE sample_index BETWEEN 2 AND 5 ORDER BY pts_seconds"}`))
	if streamed.Code != 200 {
		t.Fatal(streamed.Body.String())
	}
	streamDecoder := json.NewDecoder(streamed.Body)
	var scores []*float64
	for {
		var event struct {
			Type string
			Rows []struct {
				Score *float64 `json:"motion_score"`
			}
			Error string
		}
		if err := streamDecoder.Decode(&event); err != nil {
			t.Fatal(err)
		}
		if event.Type == "error" {
			t.Fatal(event.Error)
		}
		for _, row := range event.Rows {
			scores = append(scores, row.Score)
		}
		if event.Type == "done" {
			break
		}
	}
	if len(scores) != len(motionResult.Rows) {
		t.Fatal("stream row count differs")
	}
	for i, score := range scores {
		want := motionResult.Rows[i].Score
		if (score == nil) != (want == nil) || (score != nil && *score != *want) {
			t.Fatalf("stream row %d motion differs from full grayscale decode", i)
		}
	}
	w = request(s, "POST", "/api/query", []byte(`{"sql":"SELECT motion(frame, frame) AS zero FROM frames LIMIT 2"}`))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var zero struct{ Rows []map[string]float64 }
	if err := json.Unmarshal(w.Body.Bytes(), &zero); err != nil {
		t.Fatal(err)
	}
	for _, row := range zero.Rows {
		if row["zero"] != 0 {
			t.Fatal(row)
		}
	}
}

func TestMotionLargerSelectionAfterCachedFrames(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "large-selection.mp4")
	if _, err := command(ctx, ffmpeg, "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=32x32:rate=25", "-frames:v", "520", "-c:v", "mpeg4", "-bf", "2", path); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, path)
	for _, limit := range []int{100, 500} {
		sql := fmt.Sprintf("SELECT motion(frame, LAG(frame) OVER (PARTITION BY path, track_id ORDER BY pts_seconds, sample_index)) AS score FROM frames ORDER BY pts_seconds LIMIT %d", limit)
		body, _ := json.Marshal(map[string]string{"sql": sql})
		w := request(s, "POST", "/api/query", body)
		if w.Code != 200 {
			t.Fatalf("LIMIT %d: %s", limit, w.Body.String())
		}
		var result struct{ Rows []map[string]*float64 }
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Rows) != limit || result.Rows[0]["score"] != nil {
			t.Fatalf("LIMIT %d: incorrect result count or first value", limit)
		}
		for i, row := range result.Rows[1:] {
			score := row["score"]
			if score == nil || *score < 0 || *score > 1 {
				t.Fatalf("row %d has invalid motion score", i+1)
			}
		}
	}
}

// OFFSET must retain the preceding keyframe for LAG while decoding only the
// selected page. Compare streamed scores against a separate full movie decode.
func TestStreamMotionOverLateKeyframePage(t *testing.T) {
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
	path := filepath.Join(dir, "late-keyframes.mp4")
	if _, err := command(ctx, ffmpeg, "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=32x32:rate=25", "-frames:v", "360", "-c:v", "mpeg4", "-bf", "2", path); err != nil {
		t.Fatal(err)
	}
	baselinePath := filepath.Join(dir, "baseline.gray")
	if _, err := command(ctx, ffmpeg, "-v", "error", "-ignore_editlist", "1", "-i", path, "-an", "-vf", "scale=160:90,format=gray", "-fps_mode", "vfr", "-c:v", "rawvideo", "-pix_fmt", "gray", "-f", "rawvideo", baselinePath); err != nil {
		t.Fatal(err)
	}
	pixels, err := os.ReadFile(baselinePath)
	if err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, path)
	w := request(s, "POST", "/api/query/stream", []byte(`{"sql":"SELECT frame, pts_seconds, motion(frame, LAG(frame) OVER (PARTITION BY path, track_id ORDER BY pts_seconds, sample_index)) AS motion_score FROM frames WHERE is_keyframe = true ORDER BY path, track_id, pts_seconds LIMIT 5 OFFSET 20"}`))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	table := s.sources[0].tables["frames"]
	ordered := make([]FrameValue, table.Len())
	for i := range ordered {
		ordered[i] = table.Value(i, "frame").(FrameValue)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].PTS < ordered[j].PTS })
	keyOrdinals := []int{}
	for i, frame := range ordered {
		if frame.Keyframe {
			keyOrdinals = append(keyOrdinals, i)
		}
	}
	decoder := json.NewDecoder(w.Body)
	count, batches := 0, 0
	done := false
	for decoder.More() {
		var event struct {
			Type  string
			Error string
			Rows  []struct {
				Frame FrameValue
				Score *float64 `json:"motion_score"`
			}
		}
		if err := decoder.Decode(&event); err != nil {
			t.Fatal(err)
		}
		if event.Type == "error" {
			t.Fatal(event.Error)
		}
		if event.Type == "done" {
			done = true
		}
		if event.Type == "rows" {
			batches++
			if batches == 1 && len(event.Rows) != 1 {
				t.Fatal("first completed row was not streamed immediately")
			}
		}
		for _, row := range event.Rows {
			if count >= 5 {
				t.Fatal("too many rows")
			}
			current, previous := keyOrdinals[20+count], keyOrdinals[19+count]
			if row.Frame.SampleIndex != ordered[current].SampleIndex || row.Score == nil {
				t.Fatalf("row %d lost exact frame identity or preceding LAG value", count)
			}
			const size = 160 * 90
			delta := 0
			for i, pixel := range pixels[current*size : (current+1)*size] {
				d := int(pixel) - int(pixels[previous*size+i])
				if d < 0 {
					d = -d
				}
				delta += d
			}
			want := float64(delta) / (255 * size)
			if *row.Score != want {
				t.Fatalf("row %d score=%g want=%g", count, *row.Score, want)
			}
			count++
		}
	}
	if !done || count != 5 || batches < 2 {
		t.Fatalf("done=%v rows=%d batches=%d", done, count, batches)
	}
}
