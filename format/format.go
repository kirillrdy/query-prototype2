// Package format defines the boundary between binary formats and the query engine.
package format

import (
	"context"
	"io"
)

type Row map[string]any

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
