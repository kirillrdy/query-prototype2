package video

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Exercise FFmpeg's parser and compare actual selected timestamps, including
// sparse sets that cannot be represented as a single continuous interval.
func TestSelectExpressionExactFrames(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	cases := [][]int{{0}, {2, 3, 4, 9, 15, 16}, {100, 100, 101, 102}}
	continuous, sparse := make([]int, 500), make([]int, 500)
	for i := range continuous {
		continuous[i] = 100 + i
		sparse[i] = 2 * i
	}
	cases = append(cases, continuous, sparse)
	for _, ordinals := range cases {
		unique := []int{}
		for _, n := range ordinals {
			if len(unique) == 0 || unique[len(unique)-1] != n {
				unique = append(unique, n)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		data, err := command(ctx, ffmpeg, "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=16x16:rate=25", "-vf", "select="+selectExpression(ordinals), "-fps_mode", "passthrough", "-frames:v", strconv.Itoa(len(unique)), "-f", "framecrc", "-")
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		var got []int
		for _, line := range strings.Split(string(data), "\n") {
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			fields := strings.Split(line, ",")
			if len(fields) < 3 {
				t.Fatalf("invalid framecrc output: %q", line)
			}
			n, err := strconv.Atoi(strings.TrimSpace(fields[2]))
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, n)
		}
		if len(got) != len(unique) {
			t.Fatalf("selected %d frames, wanted %d", len(got), len(unique))
		}
		for i, n := range got {
			if n != unique[i] {
				t.Fatalf("frame %d: selected ordinal %d, wanted %d", i, n, unique[i])
			}
		}
	}
}

func TestSelectExpressionMaximumSparseBatchParses(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ordinals := make([]int, MaxFrames)
	for i := range ordinals {
		ordinals[i] = i * 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := command(ctx, ffmpeg, "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=16x16:rate=25", "-vf", "select="+selectExpression(ordinals), "-frames:v", "1", "-f", "null", "-"); err != nil {
		t.Fatal(err)
	}
}
