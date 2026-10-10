package video

import (
	"context"
	"fmt"
	"os"

	"binaryquery/engine"
	"binaryquery/format"
)

type Resolver func(any) (Reference, error)

// Motion computes normalized mean absolute grayscale difference. Null arguments
// yield null. Samples are resolved by exact byte position, then decoded in
// batches to lossless 160x90 grayscale buffers (independent of preview JPEGs).
func Motion(decoder *Decoder, resolve Resolver) engine.Function {
	return engine.Function{Arity: 2, Evaluate: func(ctx context.Context, args [][]any) ([]any, error) {
		out := make([]any, len(args))
		refs := map[string]Reference{}
		pairs := make([][2]string, len(args))
		for i, tuple := range args {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if len(tuple) != 2 {
				return nil, fmt.Errorf("expected two frames")
			}
			if tuple[0] == nil || tuple[1] == nil {
				continue
			}
			for j, value := range tuple {
				ref, err := resolve(value)
				if err != nil {
					return nil, err
				}
				refs[ref.ID] = ref
				pairs[i][j] = ref.ID
				if len(refs) > MaxFrames {
					return nil, fmt.Errorf("motion batch exceeds %d distinct frames; use a smaller LIMIT or narrower WHERE", MaxFrames)
				}
			}
		}
		type group struct {
			source Source
			track  uint32
			frames map[string]format.Frame
		}
		groups := map[string]*group{}
		for id, ref := range refs {
			key := fmt.Sprintf("%s-%d", ref.Source.Key, ref.Frame.TrackID)
			g := groups[key]
			if g == nil {
				g = &group{ref.Source, ref.Frame.TrackID, map[string]format.Frame{}}
				groups[key] = g
			}
			g.frames[id] = ref.Frame
		}
		for _, g := range groups {
			if err := decoder.Prepare(ctx, g.source, g.track, g.frames, Gray); err != nil {
				return nil, err
			}
		}
		pixels := map[string][]byte{}
		for id, ref := range refs {
			if err := ref.Source.Check(); err != nil {
				return nil, err
			}
			data, err := os.ReadFile(decoder.Path(id, Gray))
			if err != nil {
				return nil, err
			}
			if len(data) != GrayWidth*GrayHeight {
				return nil, fmt.Errorf("invalid decoded grayscale buffer")
			}
			pixels[id] = data
		}
		for i, pair := range pairs {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if pair[0] == "" {
				continue
			}
			score, err := difference(pixels[pair[0]], pixels[pair[1]])
			if err != nil {
				return nil, err
			}
			out[i] = score
		}
		return out, nil
	}}
}
func difference(a, b []byte) (float64, error) {
	if len(a) == 0 || len(a) != len(b) {
		return 0, fmt.Errorf("frame dimensions differ or are empty")
	}
	var sum uint64
	for i, x := range a {
		delta := int(x) - int(b[i])
		if delta < 0 {
			delta = -delta
		}
		sum += uint64(delta)
	}
	return float64(sum) / (255 * float64(len(a))), nil
}
