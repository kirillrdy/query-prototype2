package mp4

import (
	"encoding/binary"
	"fmt"

	"binaryquery/format"
)

// A frame row describes an encoded video sample, not a decoded image.
const maxSamples = 1000000

type run struct {
	count uint32
	value int64
}
type sampleTable struct {
	timing, composition []run
	sizes               []uint32
	sync                map[uint32]bool
	seen                map[string]bool
}
type sampleTrack struct {
	row     format.Row
	samples *sampleTable
}

func (p *parser) sampleMetadata(b box, s *sampleTable) error {
	if s.seen == nil {
		s.seen = map[string]bool{}
	}
	if s.seen[b.kind] {
		return fmt.Errorf("duplicate %s table", b.kind)
	}
	s.seen[b.kind] = true
	available := b.size - b.header
	if available < 8 {
		return fmt.Errorf("truncated sample table")
	}
	h, err := p.bytes(b.offset+b.header, 8)
	if err != nil {
		return err
	}
	if (b.kind == "ctts" && h[0] > 1) || (b.kind != "ctts" && h[0] != 0) {
		return fmt.Errorf("unsupported sample table version %d", h[0])
	}
	count := binary.BigEndian.Uint32(h[4:8])
	entrySize, header := int64(8), int64(8)
	var defaultSize uint32
	if b.kind == "stsz" {
		if available < 12 {
			return fmt.Errorf("truncated stsz header")
		}
		x, err := p.bytes(b.offset+b.header+8, 4)
		if err != nil {
			return err
		}
		defaultSize = count
		count = binary.BigEndian.Uint32(x)
		header = 12
		entrySize = 4
	} else if b.kind == "stss" {
		entrySize = 4
	}
	if count > maxSamples {
		return fmt.Errorf("%s count exceeds %d", b.kind, maxSamples)
	}
	dataSize := int64(count) * entrySize
	if b.kind == "stsz" && defaultSize != 0 {
		dataSize = 0
	}
	if dataSize > available-header {
		return fmt.Errorf("truncated %s entries", b.kind)
	}
	d, err := p.bytes(b.offset+b.header+header, dataSize)
	if err != nil {
		return err
	}
	if b.kind == "stsz" {
		s.sizes = make([]uint32, count)
	}
	if b.kind == "stss" {
		s.sync = make(map[uint32]bool, count)
	}
	var total uint64
	for i := uint32(0); i < count; i++ {
		if err := p.ctx.Err(); err != nil {
			return err
		}
		off := int(i) * int(entrySize)
		switch b.kind {
		case "stsz":
			if defaultSize != 0 {
				s.sizes[i] = defaultSize
			} else {
				s.sizes[i] = binary.BigEndian.Uint32(d[off : off+4])
			}
		case "stss":
			index := binary.BigEndian.Uint32(d[off : off+4])
			if index == 0 || s.sync[index] {
				return fmt.Errorf("invalid sync sample index %d", index)
			}
			s.sync[index] = true
		case "stts", "ctts":
			n := binary.BigEndian.Uint32(d[off : off+4])
			value := int64(binary.BigEndian.Uint32(d[off+4 : off+8]))
			if b.kind == "ctts" && h[0] == 1 {
				value = int64(int32(value))
			}
			total += uint64(n)
			if total > maxSamples {
				return fmt.Errorf("expanded %s exceeds %d samples", b.kind, maxSamples)
			}
			if b.kind == "stts" {
				s.timing = append(s.timing, run{n, value})
			} else {
				s.composition = append(s.composition, run{n, value})
			}
		}
	}
	return nil
}

func expand(runs []run, count int) ([]int64, error) {
	values := make([]int64, 0, count)
	for _, r := range runs {
		if uint64(len(values))+uint64(r.count) > uint64(count) {
			return nil, fmt.Errorf("sample table count mismatch")
		}
		for i := uint32(0); i < r.count; i++ {
			values = append(values, r.value)
		}
	}
	if len(values) != count {
		return nil, fmt.Errorf("sample table count mismatch")
	}
	return values, nil
}

func (p *parser) frames() ([]format.Row, error) {
	rows := make([]format.Row, 0)
	for _, t := range p.sampleTracks {
		if t.row["handler"] != "vide" {
			continue
		}
		s := t.samples
		// Fragmented samples live in moof/trun rather than these tables.
		if !s.seen["stsz"] && !s.seen["stts"] {
			continue
		}
		if !s.seen["stsz"] || !s.seen["stts"] {
			return nil, fmt.Errorf("video track requires both stsz and stts")
		}
		count := len(s.sizes)
		if len(rows)+count > maxSamples {
			return nil, fmt.Errorf("frame count exceeds %d per file", maxSamples)
		}
		durations, err := expand(s.timing, count)
		if err != nil {
			return nil, err
		}
		offsets := make([]int64, count)
		if s.seen["ctts"] {
			offsets, err = expand(s.composition, count)
			if err != nil {
				return nil, err
			}
		}
		for index := range s.sync {
			if uint64(index) > uint64(count) {
				return nil, fmt.Errorf("sync sample index outside sample table")
			}
		}
		if count == 0 {
			continue
		}
		scale, ok := t.row["timescale"].(uint32)
		if !ok || scale == 0 {
			return nil, fmt.Errorf("video samples require media timescale")
		}
		var dts int64
		for i, size := range s.sizes {
			if err := p.ctx.Err(); err != nil {
				return nil, err
			}
			keyframe := s.sync == nil || s.sync[uint32(i+1)]
			rows = append(rows, format.Row{"path": p.s.Name, "track_id": t.row["track_id"], "sample_index": i + 1, "dts_seconds": float64(dts) / float64(scale), "pts_seconds": float64(dts+offsets[i]) / float64(scale), "duration_seconds": float64(durations[i]) / float64(scale), "size": size, "is_keyframe": keyframe})
			dts += durations[i]
		}
	}
	return rows, nil
}
