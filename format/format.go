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
	// DecodeStart is the preceding sync sample's presentation timestamp.
	// These decoder hints stay internal; byte position remains frame identity.
	DecodeStart float64 `json:"-"`
	Seekable    bool    `json:"-"`
	Keyframe    bool    `json:"-"`
}

// Table has an explicit schema, including when it has no rows.
type Table struct {
	Columns []string
	Rows    []Row
	// Count and ValueAt expose indexed metadata without allocating a map per row.
	// Values must remain available after the source reader is closed.
	Count   int
	ValueAt func(row int, column string) any
}

func (t Table) Len() int {
	if t.ValueAt != nil {
		return t.Count
	}
	return len(t.Rows)
}
func (t Table) Value(row int, column string) any {
	if t.ValueAt != nil {
		return t.ValueAt(row, column)
	}
	return t.Rows[row][column]
}

// Append combines tables without materializing their indexed rows.
func Append(a, b Table) Table {
	if a.Columns == nil {
		return b
	}
	n := a.Len()
	return Table{Columns: a.Columns, Count: n + b.Len(), ValueAt: func(row int, column string) any {
		if row < n {
			return a.Value(row, column)
		}
		return b.Value(row-n, column)
	}}
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
