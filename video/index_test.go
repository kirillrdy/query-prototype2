package video

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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

func TestDecoderBuildsAndExtendsOnlyRequestedPrefix(t *testing.T) {
	decoder := NewDecoder(t.TempDir())
	if !decoder.Available() {
		t.Skip("ffmpeg/ffprobe unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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
	first := 0
	for _, row := range []int{4, 40} {
		frame := frames.Value(row, "frame").(format.Frame)
		id := fmt.Sprintf("sample-%d", row)
		if err := decoder.Prepare(ctx, src, frame.TrackID, map[string]format.Frame{id: frame}, Gray); err != nil {
			t.Fatal(err)
		}
		count := len(decoder.indices[fmt.Sprintf("test-%d", frame.TrackID)])
		if count <= first || count >= frames.Len() {
			t.Fatalf("requested sample %d indexed %d of %d frames (previous %d)", row+1, count, frames.Len(), first)
		}
		first = count
		// A cached representation should require no new indexing subprocess.
		probe := decoder.ffprobe
		decoder.ffprobe = "missing-probe-command"
		if err := decoder.Prepare(ctx, src, frame.TrackID, map[string]format.Frame{id: frame}, Gray); err != nil {
			t.Fatal(err)
		}
		decoder.ffprobe = probe
	}
}
