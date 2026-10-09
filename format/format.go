// Package format defines the boundary between binary formats and the query engine.
package format

import (
	"context"
	"io"
)

type Row map[string]any

// Frame is a lazy binary value: an encoded sample and its rendering identity.
// Reading it does not load the payload. Decoders need the source container,
// including codec configuration and reference frames, to produce an image.
type Frame struct {
	Type        string  `json:"type"`
	Source      string  `json:"source"`
	TrackID     uint32  `json:"track_id"`
	SampleIndex int     `json:"sample_index"`
	Offset      int64   `json:"offset"`
	Size        uint32  `json:"size"`
	PTS         float64 `json:"pts_seconds"`
	Duration    float64 `json:"duration_seconds"`
}

// Table has an explicit schema, including when it has no rows.
type Table struct {
	Columns []string
	Rows    []Row
}

type Source struct {
	Name   string
	Size   int64
	Reader io.ReaderAt
}

// Adapter exposes a binary source as relational tables. Implementations must
// validate offsets against Source.Size and respect context cancellation.
type Adapter interface {
	Name() string
	Probe(Source) bool
	Read(context.Context, Source) (map[string]Table, error)
}
