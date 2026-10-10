package video

import (
	"context"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"

	"binaryquery/engine"
	"binaryquery/format"
)

// SAM3Mask uses row-major run lengths alternating background and foreground,
// starting with background. Positive mask logits are foreground.
type SAM3Mask struct {
	Score float64  `json:"score"`
	RLE   []uint32 `json:"rle"`
}

type SAM3Result struct {
	Count  int64      `json:"count"`
	Score  float64    `json:"score"`
	Width  int        `json:"width"`
	Height int        `json:"height"`
	Masks  []SAM3Mask `json:"masks"`
}

type sam3Backend interface {
	Find(context.Context, string, string, float32) (SAM3Result, error)
	Close()
}

// Segmenter serializes inference and starts a persistent native SAM3 worker on
// first use. Its bounded cache is shared by masks, counts, and scores.
type Segmenter struct {
	decoder    *Decoder
	gate       chan struct{}
	backend    sam3Backend
	open       func() (sam3Backend, error)
	cache      map[[2]string]SAM3Result
	confidence string
	closed     bool
}

func NewSegmenter(decoder *Decoder) *Segmenter {
	confidence := os.Getenv("BQUERY_SAM3_CONFIDENCE")
	if confidence == "" {
		confidence = "0.5"
	}
	return &Segmenter{decoder: decoder, gate: make(chan struct{}, 1), open: openSAM3Backend, cache: map[[2]string]SAM3Result{}, confidence: confidence}
}

func (s *Segmenter) Close() error {
	s.gate <- struct{}{}
	defer func() { <-s.gate }()
	s.closed = true
	if s.backend != nil {
		s.backend.Close()
		s.backend = nil
	}
	clear(s.cache)
	return nil
}

func SAM3(s *Segmenter, resolve Resolver) engine.Function { return sam3Function(s, resolve, "masks") }
func SAM3Count(s *Segmenter, resolve Resolver) engine.Function {
	return sam3Function(s, resolve, "count")
}
func SAM3Score(s *Segmenter, resolve Resolver) engine.Function {
	return sam3Function(s, resolve, "score")
}

func sam3Function(s *Segmenter, resolve Resolver, output string) engine.Function {
	return engine.Function{Arity: 2, MaxBatchSize: 8, Evaluate: func(ctx context.Context, args [][]any) ([]any, error) {
		out := make([]any, len(args))
		refs := map[string]Reference{}
		keys := make([][2]string, len(args))
		for i, tuple := range args {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if len(tuple) != 2 {
				return nil, fmt.Errorf("sam3 expects a frame and a text prompt")
			}
			if tuple[0] == nil || tuple[1] == nil {
				continue
			}
			phrase, ok := tuple[1].(string)
			phrase = strings.TrimSpace(phrase)
			if !ok || phrase == "" || len(phrase) > 4096 {
				return nil, fmt.Errorf("sam3 expects a nonempty text prompt of at most 4096 bytes")
			}
			ref, err := resolve(tuple[0])
			if err != nil {
				return nil, err
			}
			if ref.ID == "" {
				return nil, fmt.Errorf("invalid empty frame ID")
			}
			refs[ref.ID] = ref
			keys[i] = [2]string{ref.ID, phrase}
			if len(refs) > MaxFrames {
				return nil, fmt.Errorf("sam3 batch exceeds %d distinct frames", MaxFrames)
			}
		}
		if len(refs) == 0 {
			return out, nil
		}
		select {
		case s.gate <- struct{}{}:
			defer func() { <-s.gate }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if s.closed {
			return nil, fmt.Errorf("sam3 segmenter is closed")
		}
		confidence, err := strconv.ParseFloat(s.confidence, 32)
		if err != nil || math.IsNaN(confidence) || math.IsInf(confidence, 0) || confidence <= 0 || confidence > 1 {
			return nil, fmt.Errorf("BQUERY_SAM3_CONFIDENCE must be greater than 0 and at most 1")
		}
		type group struct {
			source Source
			track  uint32
			frames map[string]format.Frame
		}
		groups := map[string]*group{}
		pending := map[string]bool{}
		for _, key := range keys {
			if key[0] == "" {
				continue
			}
			ref := refs[key[0]]
			if err := ref.Source.Check(); err != nil {
				clear(s.cache)
				return nil, err
			}
			if _, ok := s.cache[key]; ok {
				continue
			}
			pending[ref.ID] = true
			groupKey := fmt.Sprintf("%s-%d", ref.Source.Key, ref.Frame.TrackID)
			g := groups[groupKey]
			if g == nil {
				g = &group{ref.Source, ref.Frame.TrackID, map[string]format.Frame{}}
				groups[groupKey] = g
			}
			g.frames[ref.ID] = ref.Frame
		}
		defer func() {
			for id := range pending {
				os.Remove(s.decoder.Path(id, Segmentation))
			}
		}()
		if len(pending) > 0 {
			if s.backend == nil {
				s.backend, err = s.open()
				if err != nil {
					return nil, err
				}
			}
			for _, g := range groups {
				if err := s.decoder.Prepare(ctx, g.source, g.track, g.frames, Segmentation); err != nil {
					return nil, err
				}
			}
		}
		// Keep this batch locally even when the shared cache reaches its bound.
		batch := map[[2]string]SAM3Result{}
		for i, key := range keys {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if key[0] == "" {
				continue
			}
			result, ok := batch[key]
			if !ok {
				result, ok = s.cache[key]
			}
			if !ok {
				result, err = s.backend.Find(ctx, s.decoder.Path(key[0], Segmentation), key[1], float32(confidence))
				if err != nil {
					s.backend.Close()
					s.backend = nil
					return nil, err
				}
				if len(s.cache) >= 32 {
					clear(s.cache)
				}
				s.cache[key] = result
			}
			batch[key] = result
			switch output {
			case "count":
				out[i] = result.Count
			case "score":
				out[i] = result.Score
			default:
				out[i] = result
			}
		}
		for _, ref := range refs {
			if err := ref.Source.Check(); err != nil {
				clear(s.cache)
				return nil, err
			}
		}
		return out, nil
	}}
}
