package mp4

import (
	"encoding/binary"
	"testing"

	"binaryquery/format"
)

func locatedFixture(wide bool) []byte {
	build := func(start uint64) []byte {
		tk := make([]byte, 84)
		binary.BigEndian.PutUint32(tk[12:], 7)
		h := make([]byte, 12)
		copy(h[8:], "vide")
		var offsets []byte
		if wide {
			offsets = make([]byte, 24)
			binary.BigEndian.PutUint32(offsets[4:], 2)
			binary.BigEndian.PutUint64(offsets[8:], start)
			binary.BigEndian.PutUint64(offsets[16:], start+300)
			offsets = atom("co64", offsets)
		} else {
			offsets = atom("stco", words(0, 2, uint32(start), uint32(start+300)))
		}
		samples := atom("stbl", atom("stts", words(0, 1, 3, 40)), atom("stsz", words(0, 0, 3, 100, 200, 300)), atom("stsc", words(0, 2, 1, 2, 1, 2, 1, 1)), offsets)
		return atom("moov", atom("trak", atom("tkhd", tk), atom("mdia", atom("mdhd", timing(0, 120)), atom("hdlr", h), atom("minf", samples))))
	}
	moov := build(0)
	moov = build(uint64(len(moov) + 8))
	return append(moov, atom("mdat", make([]byte, 600))...)
}

func TestSampleLocationsAndBinaryValue(t *testing.T) {
	for _, wide := range []bool{false, true} {
		d := locatedFixture(wide)
		tables, err := read(d)
		if err != nil {
			t.Fatal(err)
		}
		frames := tables["frames"].Rows
		first := frames[0]["offset"].(int64)
		for i, delta := range []int64{0, 100, 300} {
			f := frames[i]["frame"].(format.Frame)
			if f.Offset != first+delta || f.Size != uint32((i+1)*100) || f.TrackID != 7 || f.SampleIndex != i+1 {
				t.Fatalf("frame: %+v", f)
			}
		}
	}
}

func TestInvalidSampleLocations(t *testing.T) {
	for _, mutate := range []func([]byte){
		func(d []byte) {
			for i := 0; i < len(d)-20; i++ {
				if string(d[i:i+4]) == "stco" {
					binary.BigEndian.PutUint32(d[i+12:], 1)
					return
				}
			}
		},
		func(d []byte) {
			for i := 0; i < len(d)-24; i++ {
				if string(d[i:i+4]) == "stsc" {
					binary.BigEndian.PutUint32(d[i+12:], 0)
					return
				}
			}
		},
		func(d []byte) {
			for i := 0; i < len(d)-24; i++ {
				if string(d[i:i+4]) == "stsc" {
					binary.BigEndian.PutUint32(d[i+16:], 4)
					return
				}
			}
		},
	} {
		d := locatedFixture(false)
		mutate(d)
		if _, err := read(d); err == nil {
			t.Fatal("accepted invalid byte mapping")
		}
	}
}
