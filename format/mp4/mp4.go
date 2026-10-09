// Package mp4 reads structural metadata from ISO BMFF and QuickTime files.
// Media payloads are never read or decoded.
package mp4

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"

	"binaryquery/format"
)

type Adapter struct{}

func (Adapter) Name() string { return "mp4" }

func (Adapter) Probe(s format.Source) bool {
	var h [8]byte
	if s.Size < 8 {
		return false
	}
	if _, err := s.Reader.ReadAt(h[:], 0); err != nil {
		return false
	}
	switch string(h[4:]) {
	case "ftyp", "moov", "mdat", "free", "wide", "skip":
		return true
	}
	return false
}

type box struct {
	kind, path           string
	offset, size, header int64
	depth                int
}
type parser struct {
	ctx          context.Context
	s            format.Source
	boxes        []format.Row
	tracks       []format.Row
	file         format.Row
	sampleTracks []sampleTrack
}

func (Adapter) Read(ctx context.Context, s format.Source) (map[string]format.Table, error) {
	if s.Size < 8 {
		return nil, fmt.Errorf("file is too small for an MP4 box")
	}
	p := parser{ctx: ctx, s: s, file: format.Row{"path": s.Name, "format": "mp4", "size": s.Size, "major_brand": nil, "duration_seconds": nil}}
	if err := p.walk(0, s.Size, "", 0, nil, nil); err != nil {
		return nil, err
	}
	hasMovie := false
	for _, b := range p.boxes {
		if b["type"] == "moov" && b["depth"] == 0 {
			hasMovie = true
		}
	}
	if !hasMovie {
		return nil, fmt.Errorf("missing moov box: no movie metadata available")
	}
	p.file["track_count"] = len(p.tracks)
	frames, err := p.frames()
	if err != nil {
		return nil, err
	}
	return map[string]format.Table{
		"files":  {Columns: []string{"path", "format", "size", "major_brand", "duration_seconds", "track_count"}, Rows: []format.Row{p.file}},
		"tracks": {Columns: []string{"path", "track_id", "handler", "codec", "width", "height", "timescale", "duration_seconds"}, Rows: p.tracks},
		"boxes":  {Columns: []string{"path", "box_path", "type", "offset", "size", "depth"}, Rows: p.boxes},
		"frames": {Columns: []string{"path", "track_id", "sample_index", "dts_seconds", "pts_seconds", "duration_seconds", "size", "is_keyframe"}, Rows: frames},
	}, nil
}

func (p *parser) bytes(offset, size int64) ([]byte, error) {
	if offset < 0 || size < 0 || offset > p.s.Size || size > p.s.Size-offset {
		return nil, fmt.Errorf("read outside file at %d", offset)
	}
	b := make([]byte, int(size))
	if size == 0 {
		return b, nil
	}
	_, err := p.s.Reader.ReadAt(b, offset)
	return b, err
}

func (p *parser) walk(start, end int64, parent string, depth int, track format.Row, samples *sampleTable) error {
	if depth > 32 {
		return fmt.Errorf("box nesting exceeds 32 levels")
	}
	for off := start; off < end; {
		if err := p.ctx.Err(); err != nil {
			return err
		}
		if len(p.boxes) >= 100000 {
			return fmt.Errorf("box count exceeds 100000")
		}
		if end-off < 8 {
			return fmt.Errorf("truncated box header at %d", off)
		}
		h, err := p.bytes(off, 8)
		if err != nil {
			return err
		}
		size, header := int64(binary.BigEndian.Uint32(h)), int64(8)
		kind := string(h[4:8])
		if size == 1 {
			if end-off < 16 {
				return fmt.Errorf("truncated extended box at %d", off)
			}
			x, err := p.bytes(off+8, 8)
			if err != nil {
				return err
			}
			u := binary.BigEndian.Uint64(x)
			if u > math.MaxInt64 {
				return fmt.Errorf("box size overflows at %d", off)
			}
			size, header = int64(u), 16
		} else if size == 0 {
			size = end - off
		}
		if kind == "uuid" {
			header += 16
		}
		if size < header || size > end-off {
			return fmt.Errorf("invalid %q box size %d at %d (remaining %d)", kind, size, off, end-off)
		}
		b := box{kind: kind, path: parent + "/" + kind, offset: off, size: size, header: header, depth: depth}
		p.boxes = append(p.boxes, format.Row{"path": p.s.Name, "box_path": b.path, "type": kind, "offset": off, "size": size, "depth": depth})
		if err := p.metadata(b, track, samples); err != nil {
			return fmt.Errorf("%s at %d: %w", b.path, off, err)
		}
		switch kind {
		case "moov", "mdia", "minf", "stbl", "edts", "mvex", "moof", "traf", "dinf":
			if err := p.walk(off+header, off+size, b.path, depth+1, track, samples); err != nil {
				return err
			}
		case "trak":
			t := format.Row{"path": p.s.Name, "track_id": nil, "handler": nil, "codec": nil, "width": nil, "height": nil, "timescale": nil, "duration_seconds": nil}
			st := &sampleTable{}
			if err := p.walk(off+header, off+size, b.path, depth+1, t, st); err != nil {
				return err
			}
			p.tracks = append(p.tracks, t)
			p.sampleTracks = append(p.sampleTracks, sampleTrack{t, st})
		}
		off += size
	}
	return nil
}

func (p *parser) metadata(b box, track format.Row, samples *sampleTable) error {
	switch b.kind {
	case "stts", "ctts", "stsz", "stss":
		if samples != nil {
			return p.sampleMetadata(b, samples)
		}
		return nil
	}
	n := int64(0)
	switch b.kind {
	case "ftyp":
		n = 8
	case "mvhd", "mdhd":
		n = 32
	case "tkhd":
		n = 96
	case "hdlr":
		n = 12
	case "stsd":
		n = 16
	default:
		return nil
	}
	available := b.size - b.header
	if n > available {
		n = available
	}
	d, err := p.bytes(b.offset+b.header, n)
	if err != nil {
		return err
	}
	need := func(n int) error {
		if len(d) < n {
			return fmt.Errorf("truncated metadata: need %d bytes, have %d", n, len(d))
		}
		return nil
	}
	switch b.kind {
	case "ftyp":
		if err := need(8); err != nil {
			return err
		}
		p.file["major_brand"] = string(d[:4])
	case "mvhd", "mdhd":
		if err := need(4); err != nil {
			return err
		}
		var scale uint32
		var duration uint64
		var unknown bool
		switch d[0] {
		case 0:
			if err := need(20); err != nil {
				return err
			}
			scale = binary.BigEndian.Uint32(d[12:16])
			duration = uint64(binary.BigEndian.Uint32(d[16:20]))
			unknown = duration == math.MaxUint32
		case 1:
			if err := need(32); err != nil {
				return err
			}
			scale = binary.BigEndian.Uint32(d[20:24])
			duration = binary.BigEndian.Uint64(d[24:32])
			unknown = duration == math.MaxUint64
		default:
			return fmt.Errorf("unsupported full-box version %d", d[0])
		}
		if scale == 0 {
			return fmt.Errorf("zero timescale")
		}
		target := p.file
		if b.kind == "mdhd" {
			if track == nil {
				return nil
			}
			target = track
			target["timescale"] = scale
		}
		if !unknown {
			target["duration_seconds"] = float64(duration) / float64(scale)
		}
	case "tkhd":
		if track == nil {
			return nil
		}
		if err := need(4); err != nil {
			return err
		}
		id, width := 12, 76
		if d[0] == 1 {
			id, width = 20, 88
		} else if d[0] != 0 {
			return fmt.Errorf("unsupported full-box version %d", d[0])
		}
		if err := need(width + 8); err != nil {
			return err
		}
		track["track_id"] = binary.BigEndian.Uint32(d[id : id+4])
		track["width"] = float64(binary.BigEndian.Uint32(d[width:width+4])) / 65536
		track["height"] = float64(binary.BigEndian.Uint32(d[width+4:width+8])) / 65536
	case "hdlr":
		if err := need(12); err != nil {
			return err
		}
		if track != nil {
			track["handler"] = string(d[8:12])
		}
	case "stsd":
		if err := need(8); err != nil {
			return err
		}
		if binary.BigEndian.Uint32(d[4:8]) == 0 {
			return nil
		}
		if err := need(16); err != nil {
			return err
		}
		entrySize := int64(binary.BigEndian.Uint32(d[8:12]))
		if entrySize < 8 || entrySize > available-8 {
			return fmt.Errorf("invalid sample description size")
		}
		if track != nil {
			track["codec"] = string(d[12:16])
		}
	}
	return nil
}
