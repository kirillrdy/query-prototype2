package mp4

import (
	"bytes"
	"context"
	"testing"

	"binaryquery/format"
)

func TestIndexedFrameValuesMatchMaterializedAdapter(t *testing.T) {
	d := locatedFixture(false)
	source := format.Source{Name: "test.mp4", Size: int64(len(d)), Reader: bytes.NewReader(d)}
	tables, err := (Adapter{}).ReadTables(context.Background(), source, "frames")
	if err != nil {
		t.Fatal(err)
	}
	indexed := tables["frames"]
	eager, err := (Adapter{}).Read(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if indexed.ValueAt == nil || len(indexed.Rows) != 0 || indexed.Len() != len(eager["frames"].Rows) {
		t.Fatal("expected indexed frames")
	}
	for i, row := range eager["frames"].Rows {
		for _, column := range indexed.Columns {
			if indexed.Value(i, column) != row[column] {
				t.Fatalf("row %d column %s differs", i, column)
			}
		}
	}
}

func TestMetadataQueriesSkipOversizedSampleTables(t *testing.T) {
	d := atom("moov", atom("trak", atom("stsz", words(0, 42, maxSamples+1))))
	s := format.Source{Size: int64(len(d)), Reader: bytes.NewReader(d)}
	for _, name := range []string{"files", "tracks", "boxes"} {
		tables, err := (Adapter{}).ReadTables(context.Background(), s, name)
		if err != nil {
			t.Fatalf("%s read sample metadata: %v", name, err)
		}
		if _, ok := tables["frames"]; ok {
			t.Fatal("metadata query produced frames")
		}
	}
	if _, err := (Adapter{}).ReadTables(context.Background(), s, "frames"); err == nil {
		t.Fatal("frame indexing should retain sample bounds")
	}
}
