package video

import (
	"context"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"

	"binaryquery/engine"
	"binaryquery/format"
)

// detectionBackend performs local inference. Implementations own model resources.
type detectionBackend interface {
	Count(context.Context, string, float32) (map[string]int, error)
	Close()
}

const DetectionBatchSize = 32

// Detector lazily initializes an automatically cached model and runtime.
type Detector struct {
	decoder                     *Decoder
	model, cacheDir, confidence string
	gate                        chan struct{}
	cache                       map[string]map[string]int
	backend                     detectionBackend
	open                        func(context.Context, string, string) (detectionBackend, error)
	closed                      bool
}

func NewDetector(decoder *Decoder) *Detector {
	confidence := os.Getenv("BQUERY_DETECTION_CONFIDENCE")
	if confidence == "" {
		confidence = "0.25"
	}
	return &Detector{decoder: decoder, model: os.Getenv("BQUERY_DETECTION_MODEL"), cacheDir: os.Getenv("BQUERY_DETECTION_CACHE"), confidence: confidence, gate: make(chan struct{}, 1), cache: map[string]map[string]int{}, open: openDetectionBackend}
}

func (d *Detector) Close() error {
	d.gate <- struct{}{}
	defer func() { <-d.gate }()
	d.closed = true
	if d.backend != nil {
		d.backend.Close()
		d.backend = nil
	}
	clear(d.cache)
	return nil
}

func (d *Detector) start(ctx context.Context) (float32, error) {
	confidence, err := strconv.ParseFloat(d.confidence, 32)
	if err != nil || math.IsNaN(confidence) || math.IsInf(confidence, 0) || confidence <= 0 || confidence > 1 {
		return 0, fmt.Errorf("BQUERY_DETECTION_CONFIDENCE must be a number greater than 0 and at most 1")
	}
	if d.backend == nil {
		d.backend, err = d.open(ctx, d.model, d.cacheDir)
		if err != nil {
			return 0, err
		}
	}
	return float32(confidence), nil
}

// PersonCount returns the number of detected people, or null for a null frame.
func PersonCount(d *Detector, resolve Resolver) engine.Function { return objectCount(d, resolve, true) }

// ObjectCount takes a frame and a model class name, such as 'person' or 'car'.
// Unknown labels fail instead of silently returning zero for misspellings.
func ObjectCount(d *Detector, resolve Resolver) engine.Function {
	return objectCount(d, resolve, false)
}

func objectCount(d *Detector, resolve Resolver, person bool) engine.Function {
	arity := 2
	if person {
		arity = 1
	}
	return engine.Function{Arity: arity, MaxBatchSize: DetectionBatchSize, Evaluate: func(ctx context.Context, args [][]any) ([]any, error) {
		out := make([]any, len(args))
		refs := map[string]Reference{}
		ids, labels := make([]string, len(args)), make([]string, len(args))
		for i, tuple := range args {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if len(tuple) != arity {
				return nil, fmt.Errorf("object count expects %d arguments", arity)
			}
			if tuple[0] == nil || (!person && tuple[1] == nil) {
				continue
			}
			label := "person"
			if !person {
				var ok bool
				label, ok = tuple[1].(string)
				if !ok || strings.TrimSpace(label) == "" {
					return nil, fmt.Errorf("object_count expects a nonempty class name string")
				}
				label = strings.ToLower(strings.TrimSpace(label))
			}
			ref, err := resolve(tuple[0])
			if err != nil {
				return nil, err
			}
			if ref.ID == "" {
				return nil, fmt.Errorf("invalid empty frame ID")
			}
			refs[ref.ID] = ref
			ids[i], labels[i] = ref.ID, label
			if len(refs) > MaxFrames {
				return nil, fmt.Errorf("object detection batch exceeds %d distinct frames; use a smaller LIMIT or narrower WHERE", MaxFrames)
			}
		}
		if len(refs) == 0 {
			return out, nil
		}
		select {
		case d.gate <- struct{}{}:
			defer func() { <-d.gate }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if d.closed {
			return nil, fmt.Errorf("object detector is closed")
		}
		pending := []string{}
		type group struct {
			source Source
			track  uint32
			frames map[string]format.Frame
		}
		groups := map[string]*group{}
		for id, ref := range refs {
			if err := ref.Source.Check(); err != nil {
				return nil, err
			}
			if _, ok := d.cache[id]; ok {
				continue
			}
			pending = append(pending, id)
			key := fmt.Sprintf("%s-%d", ref.Source.Key, ref.Frame.TrackID)
			g := groups[key]
			if g == nil {
				g = &group{ref.Source, ref.Frame.TrackID, map[string]format.Frame{}}
				groups[key] = g
			}
			g.frames[id] = ref.Frame
		}
		if len(pending) > 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			confidence, err := d.start(ctx)
			if err != nil {
				return nil, err
			}
			// Analysis images have their own namespace, so releasing each batch
			// cannot race preview requests or fill the disk during long scans.
			defer func() {
				for _, id := range pending {
					os.Remove(d.decoder.Path(id, Detection))
				}
			}()
			for _, g := range groups {
				if err := d.decoder.Prepare(ctx, g.source, g.track, g.frames, Detection); err != nil {
					return nil, err
				}
			}
			sort.Strings(pending)
			// Retain this call's hits locally if bounded cache eviction is needed.
			if len(d.cache)+len(pending) > MaxFrames {
				keep := map[string]map[string]int{}
				for id := range refs {
					if counts, ok := d.cache[id]; ok {
						keep[id] = counts
					}
				}
				d.cache = keep
			}
			for _, id := range pending {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				counts, err := d.backend.Count(ctx, d.decoder.Path(id, Detection), confidence)
				if err != nil {
					return nil, err
				}
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				d.cache[id] = counts
			}
		}
		for id, ref := range refs {
			if err := ref.Source.Check(); err != nil {
				delete(d.cache, id)
				return nil, err
			}
		}
		for i, id := range ids {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if id == "" {
				continue
			}
			count, ok := d.cache[id][labels[i]]
			if !ok {
				return nil, fmt.Errorf("unknown object class %q for this model", labels[i])
			}
			out[i] = int64(count)
		}
		return out, nil
	}}
}
