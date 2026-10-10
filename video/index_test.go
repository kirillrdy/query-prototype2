package video

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"binaryquery/format"
	"binaryquery/format/mp4"
)

// No closing array is provided: the reader would fail if indexing waited for
// the whole track instead of returning once the requested samples are found.
func TestSampleIndexStopsAtRequestedPrefixAndExtends(t *testing.T) {
	index := map[int64]int{}
	wanted := map[int64]bool{200: true}
	err := readSampleIndex(context.Background(), json.NewDecoder(strings.NewReader(`{"frames":[{"pkt_pos":"100"},{"pkt_pos":"200"},`)), wanted, index)
	if err != nil || len(wanted) != 0 || index[100] != 0 || index[200] != 1 {
		t.Fatalf("prefix: %v %v", index, err)
	}
	wanted = map[int64]bool{300: true}
	err = readSampleIndex(context.Background(), json.NewDecoder(strings.NewReader(`{"frames":[{"pkt_pos":"100"},{"pkt_pos":"200"},{"pkt_pos":"300"},`)), wanted, index)
	if err != nil || index[100] != 0 || index[200] != 1 || index[300] != 2 {
		t.Fatalf("extension: %v %v", index, err)
	}
	if err := readSampleIndex(context.Background(), json.NewDecoder(strings.NewReader(`{"frames":[]}`)), map[int64]bool{900: true}, index); err == nil || err == io.EOF {
		t.Fatalf("missing frame: %v", err)
	}
}

func TestDecoderSeeksToSamplesAndSkipsInterFrames(t *testing.T) {
	decoder := NewDecoder(t.TempDir())
	if !decoder.Available() {
		t.Skip("ffmpeg/ffprobe unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "video.mp4")
	if output, err := exec.CommandContext(ctx, decoder.ffmpeg, "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=32x32:rate=25", "-frames:v", "120", "-c:v", "mpeg4", "-bf", "2", path).CombinedOutput(); err != nil {
		t.Fatalf("%s: %v", output, err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	tables, err := (mp4.Adapter{}).ReadTables(ctx, format.Source{Name: path, Size: stat.Size(), Reader: f}, "frames")
	if err != nil {
		t.Fatal(err)
	}
	frames := tables["frames"]
	src := Source{Key: "test", Path: path, Size: stat.Size(), Modified: stat.ModTime()}
	// An independent full decode provides exact pixels in presentation order.
	baseline := filepath.Join(t.TempDir(), "baseline.gray")
	if output, err := exec.CommandContext(ctx, decoder.ffmpeg, "-v", "error", "-ignore_editlist", "1", "-i", path, "-an", "-vf", "scale=160:90,format=gray", "-fps_mode", "vfr", "-c:v", "rawvideo", "-pix_fmt", "gray", "-f", "rawvideo", baseline).CombinedOutput(); err != nil {
		t.Fatalf("%s: %v", output, err)
	}
	pixels, err := os.ReadFile(baseline)
	if err != nil {
		t.Fatal(err)
	}
	ordered := make([]format.Frame, frames.Len())
	for i := range ordered {
		ordered[i] = frames.Value(i, "frame").(format.Frame)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].PTS < ordered[j].PTS })
	ordinals := map[int]int{}
	for i, f := range ordered {
		ordinals[f.SampleIndex] = i
	}
	check := func(id string, f format.Frame) {
		t.Helper()
		actual, err := os.ReadFile(decoder.Path(id, Gray))
		if err != nil {
			t.Fatal(err)
		}
		start := ordinals[f.SampleIndex] * GrayWidth * GrayHeight
		if !bytes.Equal(actual, pixels[start:start+GrayWidth*GrayHeight]) {
			t.Fatalf("sample %d rendered the wrong picture after seeking", f.SampleIndex)
		}
	}
	for _, row := range []int{4, 40, 119} {
		frame := frames.Value(row, "frame").(format.Frame)
		id := fmt.Sprintf("sample-%d", row)
		if err := decoder.Prepare(ctx, src, frame.TrackID, map[string]format.Frame{id: frame}, Gray); err != nil {
			t.Fatal(err)
		}
		check(id, frame)
		// A cached representation should require no new indexing subprocess.
		probe := decoder.ffprobe
		decoder.ffprobe = "missing-probe-command"
		if err := decoder.Prepare(ctx, src, frame.TrackID, map[string]format.Frame{id: frame}, Gray); err != nil {
			t.Fatal(err)
		}
		decoder.ffprobe = probe
	}
	for key, index := range decoder.indices {
		if len(index) > 30 {
			t.Fatalf("seek %s indexed %d frames instead of a bounded GOP", key, len(index))
		}
	}
	// A cold batch of late keyframes must index keyframes only, not the prefix.
	decoder = NewDecoder(t.TempDir())
	selected := map[string]format.Frame{}
	for i := 60; i < frames.Len(); i++ {
		f := frames.Value(i, "frame").(format.Frame)
		if f.Keyframe {
			selected[fmt.Sprintf("key-%d", i)] = f
		}
	}
	if err := decoder.Prepare(ctx, src, 1, selected, Gray); err != nil {
		t.Fatal(err)
	}
	for id, f := range selected {
		check(id, f)
	}
	for key, index := range decoder.indices {
		if len(index) > len(selected)+1 {
			t.Fatalf("keyframe seek %s indexed %d frames for %d keys", key, len(index), len(selected))
		}
	}
}
