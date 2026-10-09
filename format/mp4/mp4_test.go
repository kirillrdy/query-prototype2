package mp4

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"testing"

	"binaryquery/format"
)

func atom(kind string, payload ...[]byte) []byte {
	d := make([]byte, 8)
	copy(d[4:], kind)
	for _, p := range payload {
		d = append(d, p...)
	}
	binary.BigEndian.PutUint32(d, uint32(len(d)))
	return d
}
func words(w ...uint32) []byte {
	d := make([]byte, len(w)*4)
	for i, n := range w {
		binary.BigEndian.PutUint32(d[i*4:], n)
	}
	return d
}
func timing(version byte, duration uint64) []byte {
	if version == 0 {
		d := make([]byte, 20)
		binary.BigEndian.PutUint32(d[12:], 1000)
		binary.BigEndian.PutUint32(d[16:], uint32(duration))
		return d
	}
	d := make([]byte, 32)
	d[0] = version
	binary.BigEndian.PutUint32(d[20:], 1000)
	binary.BigEndian.PutUint64(d[24:], duration)
	return d
}
func fixture(version byte, composition []byte) []byte {
	tk := make([]byte, 84)
	id, width := 12, 76
	if version == 1 {
		tk = make([]byte, 96)
		tk[0] = 1
		id, width = 20, 88
	}
	binary.BigEndian.PutUint32(tk[id:], 7)
	binary.BigEndian.PutUint32(tk[width:], 1920<<16)
	binary.BigEndian.PutUint32(tk[width+4:], 1080<<16)
	handler := make([]byte, 12)
	copy(handler[8:], "vide")
	stbl := atom("stbl", atom("stts", words(0, 2, 2, 40, 1, 80)), atom("stsz", words(0, 0, 3, 100, 200, 300)), atom("stss", words(0, 1, 1)), composition)
	return atom("moov", atom("mvhd", timing(version, 160)), atom("trak", atom("tkhd", tk), atom("mdia", atom("mdhd", timing(version, 160)), atom("hdlr", handler), atom("minf", stbl))))
}
func read(d []byte) (map[string]format.Table, error) {
	return (Adapter{}).Read(context.Background(), format.Source{Name: "test.mp4", Size: int64(len(d)), Reader: bytes.NewReader(d)})
}

func TestFramesAndHeaderVersions(t *testing.T) {
	for _, version := range []byte{0, 1} {
		tables, err := read(fixture(version, atom("ctts", words(1<<24, 2, 2, 20, 1, math.MaxUint32-19))))
		if err != nil {
			t.Fatal(err)
		}
		track := tables["tracks"].Rows[0]
		if track["width"] != float64(1920) || track["track_id"] != uint32(7) {
			t.Fatalf("track: %v", track)
		}
		frames := tables["frames"].Rows
		if len(frames) != 3 {
			t.Fatalf("frames: %v", frames)
		}
		if frames[0]["dts_seconds"] != float64(0) || frames[0]["pts_seconds"] != 0.02 || frames[2]["dts_seconds"] != 0.08 || frames[2]["pts_seconds"] != 0.06 || frames[2]["duration_seconds"] != 0.08 || frames[2]["size"] != uint32(300) || frames[2]["is_keyframe"] != false {
			t.Fatalf("frames: %v", frames)
		}
	}
}

func TestBoxSizesAndMalformedInput(t *testing.T) {
	extended := append(words(1), []byte("free")...)
	extended = append(extended, make([]byte, 8)...)
	binary.BigEndian.PutUint64(extended[8:], 16)
	zero := atom("mdat", []byte("ignored payload"))
	binary.BigEndian.PutUint32(zero, 0)
	if _, err := read(bytes.Join([][]byte{extended, fixture(0, nil), zero}, nil)); err != nil {
		t.Fatal(err)
	}
	cases := [][]byte{nil, words(7, 0), atom("moov", []byte{1}), atom("uuid"), atom("moov", atom("mvhd", []byte{0})), atom("moov", atom("mvhd", make([]byte, 20))), atom("moov", atom("trak", atom("stsz", words(0, 0, maxSamples+1))))}
	for _, d := range cases {
		if _, err := read(d); err == nil {
			t.Fatalf("accepted malformed input %x", d)
		}
	}
	deep := atom("free")
	for i := 0; i < 34; i++ {
		deep = atom("moov", deep)
	}
	if _, err := read(deep); err == nil {
		t.Fatal("accepted excessive nesting")
	}
}

func TestSampleMismatchAndUnknownDuration(t *testing.T) {
	bad := atom("ctts", words(0, 1, 2, 20))
	if _, err := read(fixture(0, bad)); err == nil {
		t.Fatal("accepted mismatched composition sample count")
	}
	tables, err := read(atom("moov", atom("mvhd", timing(1, math.MaxUint64))))
	if err != nil {
		t.Fatal(err)
	}
	if tables["files"].Rows[0]["duration_seconds"] != nil {
		t.Fatal("unknown duration must be null")
	}
	if len(tables["frames"].Rows) != 0 {
		t.Fatal("expected no frames")
	}
}

func TestFixedSampleSizeAndAbsentSyncTable(t *testing.T) {
	handler := make([]byte, 12)
	copy(handler[8:], "vide")
	d := atom("moov", atom("trak", atom("mdia", atom("mdhd", timing(0, 120)), atom("hdlr", handler), atom("minf", atom("stbl", atom("stts", words(0, 1, 3, 40)), atom("stsz", words(0, 42, 3)))))))
	tables, err := read(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range tables["frames"].Rows {
		if r["size"] != uint32(42) || r["is_keyframe"] != true {
			t.Fatalf("frame: %v", r)
		}
	}
	if len(tables["frames"].Rows) != 3 {
		t.Fatal("incorrect sample count")
	}
}

type guardedReader struct {
	data       []byte
	mediaStart int64
}

func (r guardedReader) ReadAt(p []byte, off int64) (int, error) {
	if off >= r.mediaStart+8 && len(p) > 0 {
		return 0, errors.New("read media payload")
	}
	return bytes.NewReader(r.data).ReadAt(p, off)
}
func TestSkipsMediaAndCancellation(t *testing.T) {
	d := fixture(0, nil)
	start := len(d)
	d = append(d, atom("mdat", make([]byte, 4096))...)
	s := format.Source{Size: int64(len(d)), Reader: guardedReader{d, int64(start)}}
	if _, err := (Adapter{}).Read(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (Adapter{}).Read(ctx, s); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}

func FuzzRead(f *testing.F) {
	f.Add(fixture(0, nil))
	f.Add(atom("moov"))
	f.Add([]byte("invalid"))
	f.Fuzz(func(t *testing.T, d []byte) { _, _ = read(d) })
}
