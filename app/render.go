package app

import (
	"binaryquery/format"
	"binaryquery/video"
	"context"
	"fmt"
)

type renderer struct{ decoder *video.Decoder }

func newRenderer(temp string) *renderer { return &renderer{video.NewDecoder(temp)} }
func (r *renderer) available() bool     { return r.decoder.Available() }
func (src *source) decodeSource() video.Source {
	return video.Source{Key: src.id, Path: src.path, Size: src.size, Modified: src.modified}
}
func (r *renderer) prepare(ctx context.Context, src *source, track uint32, frames map[string]format.Frame) error {
	return r.decoder.Prepare(ctx, src.decodeSource(), track, frames, video.Preview)
}
func (s *Server) resolveFrame(value any) (video.Reference, error) {
	frame, ok := value.(FrameValue)
	if !ok {
		return video.Reference{}, fmt.Errorf("motion expects frame values, got %T", value)
	}
	src, data, err := s.lookup(frame.ID)
	if err != nil {
		return video.Reference{}, err
	}
	return video.Reference{ID: frame.ID, Source: src.decodeSource(), Frame: data}, nil
}
