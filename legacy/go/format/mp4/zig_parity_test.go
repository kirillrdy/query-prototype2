package mp4

import (
	"binaryquery/engine"
	"binaryquery/format"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func TestZigMP4Parity(t *testing.T) {
	native := os.Getenv("BQUERY_ZIG")
	if native == "" {
		native, _ = filepath.Abs("../../../../zig-out/bin/bquery")
		if _, err := os.Stat(native); err != nil {
			t.Skip("build the native CLI with zig build")
		}
	}
	native, _ = filepath.Abs(native)
	fixtures := [][]byte{fixture(0, nil), fixture(1, atom("ctts", words(1<<24, 2, 2, 20, 1, math.MaxUint32-19))), locatedFixture(false), locatedFixture(true), atom("moov"), atom("moov", atom("mvhd", timing(1, math.MaxUint64))), nil, words(7, 0), atom("moov", []byte{1}), atom("uuid"), atom("moov", atom("mvhd", []byte{0})), atom("moov", atom("mvhd", make([]byte, 20))), fixture(0, atom("ctts", words(0, 1, 2, 20))), atom("moov", atom("trak", atom("stsz", words(0, 0, maxSamples+1))))}
	extended := append(words(1), []byte("free")...)
	extended = append(extended, make([]byte, 8)...)
	binary2 := uint64(16)
	binary.BigEndian.PutUint64(extended[8:], binary2)
	fixtures = append(fixtures, append(extended, fixture(0, nil)...))
	count := 0
	for _, data := range fixtures {
		path := filepath.Join(t.TempDir(), "test.mp4")
		os.WriteFile(path, data, 0600)
		for _, table := range []string{"files", "tracks", "boxes", "frames"} {
			tables, err := (Adapter{}).ReadTables(context.Background(), format.Source{Name: path, Size: int64(len(data)), Reader: bytes.NewReader(data)}, table)
			sql := "SELECT * FROM " + table
			var expected engine.Result
			if err == nil {
				query, _ := engine.Parse(sql)
				expected, err = query.Execute(context.Background(), tables)
			}
			output, nativeErr := exec.Command(native, "-query", sql, path).Output()
			if (err == nil) != (nativeErr == nil) {
				t.Fatalf("acceptance mismatch for fixture %d table %s: Go %v Zig %v", count, table, err, nativeErr)
			}
			if err == nil {
				want, _ := json.Marshal(expected)
				var x, y any
				json.Unmarshal(want, &x)
				json.Unmarshal(output, &y)
				if !reflect.DeepEqual(x, y) {
					t.Fatalf("%s mismatch\nGo: %s\nZig: %s", table, want, output)
				}
			}
			count++
		}
	}
	t.Logf("compared %d MP4 queries including malformed fixtures", count)
}
