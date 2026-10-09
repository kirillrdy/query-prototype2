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
	"strconv"
	"strings"

	"binaryquery/format"
)

type renderer struct {
	ffmpeg, ffprobe, temp string
	gate                  chan struct{}
	indices               map[string]map[int64]int
}

func newRenderer(temp string) *renderer {
	ffmpeg, _ := exec.LookPath("ffmpeg")
	ffprobe, _ := exec.LookPath("ffprobe")
	return &renderer{ffmpeg: ffmpeg, ffprobe: ffprobe, temp: temp, gate: make(chan struct{}, 1), indices: map[string]map[int64]int{}}
}
func (r *renderer) available() bool { return r.ffmpeg != "" && r.ffprobe != "" }

// Commands use argument arrays, never a shell. Output is bounded even for
// unexpected decoders; cancellation terminates the child process.
type boundedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(data []byte) (int, error) {
	if len(data) > b.limit-b.Len() {
		return 0, fmt.Errorf("decoder output exceeds limit")
	}
	return b.Buffer.Write(data)
}
func command(ctx context.Context, program string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, program, args...)
	out := &boundedBuffer{limit: 64 << 20}
	stderr := &boundedBuffer{limit: 1 << 20}
	cmd.Stdout = out
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		message := strings.TrimSpace(stderr.String())
		if len(message) > 1000 {
			message = message[:1000]
		}
		return nil, fmt.Errorf("decoder failed: %v %s", err, message)
	}
	return out.Bytes(), nil
}

func (r *renderer) prepare(ctx context.Context, src *source, track uint32, frames map[string]format.Frame) error {
	if !r.available() {
		return fmt.Errorf("frame previews require ffmpeg and ffprobe on PATH; binary download and metadata queries still work")
	}
	select {
	case r.gate <- struct{}{}:
		defer func() { <-r.gate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := src.unchanged(); err != nil {
		return err
	}
	pending := map[string]format.Frame{}
	for id, f := range frames {
		if _, err := os.Stat(filepath.Join(r.temp, id+".jpg")); err != nil {
			pending[id] = f
		}
	}
	if len(pending) == 0 {
		return nil
	}
	key := fmt.Sprintf("%s-%d", src.id, track)
	index := r.indices[key]
	if index == nil {
		// pkt_pos maps decoded pictures to exact source samples, including B
		// frames. A nearest-timestamp seek cannot provide this guarantee.
		data, err := command(ctx, r.ffprobe, "-v", "error", "-ignore_editlist", "1", "-select_streams", fmt.Sprintf("i:%d", track), "-show_frames", "-show_entries", "frame=pkt_pos", "-of", "json", src.path)
		if err != nil {
			return err
		}
		var result struct {
			Frames []struct {
				Position string `json:"pkt_pos"`
			} `json:"frames"`
		}
		if err := json.Unmarshal(data, &result); err != nil {
			return err
		}
		index = map[int64]int{}
		for i, frame := range result.Frames {
			position, err := strconv.ParseInt(frame.Position, 10, 64)
			if err != nil {
				continue
			}
			if _, exists := index[position]; exists {
				index[position] = -1
			} else {
				index[position] = i
			}
		}
		r.indices[key] = index
	}
	type selection struct {
		id      string
		ordinal int
	}
	selected := make([]selection, 0, len(pending))
	for id, frame := range pending {
		ordinal, ok := index[frame.Offset]
		if !ok || ordinal < 0 {
			return fmt.Errorf("sample %d cannot be mapped to exactly one decoded frame", frame.SampleIndex)
		}
		selected = append(selected, selection{id, ordinal})
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].ordinal < selected[j].ordinal })
	expressions := make([]string, len(selected))
	for i := range selected {
		expressions[i] = fmt.Sprintf("eq(n\\,%d)", selected[i].ordinal)
	}
	staging, err := os.MkdirTemp(r.temp, "render-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	filter := "select=" + strings.Join(expressions, "+") + ",scale=w='min(1280,iw)':h='min(720,ih)':force_original_aspect_ratio=decrease"
	_, err = command(ctx, r.ffmpeg, "-v", "error", "-nostdin", "-ignore_editlist", "1", "-i", src.path, "-map", fmt.Sprintf("0:i:%d", track), "-an", "-vf", filter, "-fps_mode", "vfr", "-frames:v", strconv.Itoa(len(selected)), "-q:v", "2", filepath.Join(staging, "%06d.jpg"))
	if err != nil {
		return err
	}
	if err := src.unchanged(); err != nil {
		return err
	}
	for i := range selected {
		if _, err := os.Stat(filepath.Join(staging, fmt.Sprintf("%06d.jpg", i+1))); err != nil {
			return fmt.Errorf("decoder produced fewer frames than selected")
		}
	}
	for i, f := range selected {
		if err := os.Rename(filepath.Join(staging, fmt.Sprintf("%06d.jpg", i+1)), filepath.Join(r.temp, f.id+".jpg")); err != nil {
			return err
		}
	}
	return nil
}
