package video

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

type Decoder struct {
	ffmpeg, ffprobe, temp string
	gate                  chan struct{}
	indices               map[string]map[int64]int
}

func NewDecoder(temp string) *Decoder {
	ffmpeg, _ := exec.LookPath("ffmpeg")
	ffprobe, _ := exec.LookPath("ffprobe")
	return &Decoder{ffmpeg: ffmpeg, ffprobe: ffprobe, temp: temp, gate: make(chan struct{}, 1), indices: map[string]map[int64]int{}}
}
func (r *Decoder) Available() bool { return r.ffmpeg != "" && r.ffprobe != "" }

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

func (r *Decoder) Prepare(ctx context.Context, src Source, track uint32, frames map[string]format.Frame, representation Representation) error {
	suffix, filterSuffix, codecArgs, err := representation.options()
	if err != nil {
		return err
	}
	if len(frames) > MaxFrames {
		return fmt.Errorf("decode batch exceeds %d frames; use a smaller LIMIT", MaxFrames)
	}
	for id := range frames {
		if id == "" || strings.ContainsAny(id, "/\\") || id == ".." {
			return fmt.Errorf("invalid frame cache key")
		}
	}
	if !r.Available() {
		return fmt.Errorf("frame decoding requires ffmpeg and ffprobe on PATH")
	}
	select {
	case r.gate <- struct{}{}:
		defer func() { <-r.gate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := src.Check(); err != nil {
		return err
	}
	pending := map[string]format.Frame{}
	for id, f := range frames {
		if _, err := os.Stat(filepath.Join(r.temp, id+suffix)); err != nil {
			pending[id] = f
		}
	}
	if len(pending) == 0 {
		return nil
	}
	key := fmt.Sprintf("%s-%d", src.Key, track)
	index := r.indices[key]
	if index == nil {
		index = map[int64]int{}
		r.indices[key] = index
	}
	if err := r.indexSamples(ctx, src, track, pending, index); err != nil {
		return err
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
	ordinals := make([]int, len(selected))
	for i := range selected {
		ordinals[i] = selected[i].ordinal
	}
	staging, err := os.MkdirTemp(r.temp, "render-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	filter := "select=" + selectExpression(ordinals) + filterSuffix
	args := []string{"-v", "error", "-nostdin", "-ignore_editlist", "1", "-i", src.Path, "-map", fmt.Sprintf("0:i:%d", track), "-an", "-vf", filter, "-fps_mode", "vfr", "-frames:v", strconv.Itoa(len(selected))}
	args = append(args, codecArgs...)
	args = append(args, filepath.Join(staging, "%06d"+suffix))
	_, err = command(ctx, r.ffmpeg, args...)
	if err != nil {
		return err
	}
	if err := src.Check(); err != nil {
		return err
	}
	for i := range selected {
		if _, err := os.Stat(filepath.Join(staging, fmt.Sprintf("%06d"+suffix, i+1))); err != nil {
			return fmt.Errorf("decoder produced fewer frames than selected")
		}
	}
	for i, f := range selected {
		if err := os.Rename(filepath.Join(staging, fmt.Sprintf("%06d"+suffix, i+1)), filepath.Join(r.temp, f.id+suffix)); err != nil {
			return err
		}
	}
	return nil
}

// indexSamples reads FFprobe's JSON incrementally and terminates decoding as
// soon as every requested byte position is mapped. Later requests extend the
// cached prefix. pkt_pos retains exact B-frame identity, unlike timestamp seeks.
func (r *Decoder) indexSamples(ctx context.Context, src Source, track uint32, frames map[string]format.Frame, index map[int64]int) error {
	wanted := map[int64]bool{}
	for _, f := range frames {
		if _, ok := index[f.Offset]; !ok {
			wanted[f.Offset] = true
		}
	}
	if len(wanted) == 0 {
		return nil
	}
	probeCtx, stop := context.WithCancel(ctx)
	defer stop()
	cmd := exec.CommandContext(probeCtx, r.ffprobe, "-v", "error", "-ignore_editlist", "1", "-select_streams", fmt.Sprintf("i:%d", track), "-show_frames", "-show_entries", "frame=pkt_pos", "-of", "json", src.Path)
	stderr := &boundedBuffer{limit: 1 << 20}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	decodeErr := readSampleIndex(ctx, json.NewDecoder(stdout), wanted, index)
	// Reap the process on every path, including early success and cancellation.
	stop()
	stdout.Close()
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if decodeErr != nil {
		return fmt.Errorf("frame indexing failed: %w %s", decodeErr, strings.TrimSpace(stderr.String()))
	}
	if len(wanted) != 0 {
		return fmt.Errorf("requested samples not found in decoded track: %v %s", waitErr, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func readSampleIndex(ctx context.Context, decoder *json.Decoder, wanted map[int64]bool, index map[int64]int) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if token != json.Delim('{') {
		return fmt.Errorf("expected frame index object")
	}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return err
		}
		if key != "frames" {
			var skip json.RawMessage
			if err := decoder.Decode(&skip); err != nil {
				return err
			}
			continue
		}
		if token, err := decoder.Token(); err != nil || token != json.Delim('[') {
			return fmt.Errorf("expected frames array: %v", err)
		}
		ordinal := 0
		for decoder.More() {
			if err := ctx.Err(); err != nil {
				return err
			}
			var frame struct {
				Position string `json:"pkt_pos"`
			}
			if err := decoder.Decode(&frame); err != nil {
				return err
			}
			position, err := strconv.ParseInt(frame.Position, 10, 64)
			if err == nil {
				if previous, exists := index[position]; exists && previous != ordinal {
					index[position] = -1
				} else {
					index[position] = ordinal
				}
				delete(wanted, position)
			}
			ordinal++
			if len(wanted) == 0 {
				return nil
			}
		}
		return fmt.Errorf("requested samples not found in decoded track")
	}
	return fmt.Errorf("missing frames array")
}
