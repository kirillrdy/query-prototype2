package mp4

import (
	"fmt"
	"sort"
)

type byteRange struct{ start, end int64 }

func (p *parser) sampleLocations(s *sampleTable) ([]int64, error) {
	if !s.seen["stsc"] && !s.seen["stco"] && !s.seen["co64"] {
		return nil, nil
	}
	if !s.seen["stsc"] || (!s.seen["stco"] && !s.seen["co64"]) || (s.seen["stco"] && s.seen["co64"]) {
		return nil, fmt.Errorf("incomplete or conflicting sample location tables")
	}
	if len(s.sizes) == 0 {
		if len(s.chunks) != 0 || len(s.chunkRuns) != 0 {
			return nil, fmt.Errorf("nonempty chunk table for empty samples")
		}
		return []int64{}, nil
	}
	if len(s.chunkRuns) == 0 || len(s.chunks) == 0 || uint64(s.chunkRuns[len(s.chunkRuns)-1].first) > uint64(len(s.chunks)) {
		return nil, fmt.Errorf("sample-to-chunk mapping outside chunk table")
	}
	locations := make([]int64, 0, len(s.sizes))
	runIndex := 0
	for i, chunk := range s.chunks {
		if err := p.ctx.Err(); err != nil {
			return nil, err
		}
		for runIndex+1 < len(s.chunkRuns) && uint64(s.chunkRuns[runIndex+1].first) <= uint64(i+1) {
			runIndex++
		}
		n := s.chunkRuns[runIndex].samples
		if uint64(len(locations))+uint64(n) > uint64(len(s.sizes)) {
			return nil, fmt.Errorf("chunk mapping exceeds sample count")
		}
		for j := uint32(0); j < n; j++ {
			size := uint64(s.sizes[len(locations)])
			if chunk > uint64(p.s.Size) || size > uint64(p.s.Size)-chunk {
				return nil, fmt.Errorf("sample byte range outside source")
			}
			position := int64(chunk)
			r := sort.Search(len(p.mediaRanges), func(i int) bool { return p.mediaRanges[i].end > position })
			if r == len(p.mediaRanges) || position < p.mediaRanges[r].start || int64(size) > p.mediaRanges[r].end-position {
				return nil, fmt.Errorf("sample byte range outside media data")
			}
			locations = append(locations, int64(chunk))
			chunk += size
		}
	}
	if len(locations) != len(s.sizes) {
		return nil, fmt.Errorf("chunk mapping does not cover samples")
	}
	return locations, nil
}
