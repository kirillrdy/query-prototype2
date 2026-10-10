package video

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"binaryquery/engine"
	"binaryquery/format"
)

type fakeDetector struct {
	paths  []string
	err    error
	closed bool
}

func (f *fakeDetector) Close() { f.closed = true }
func (f *fakeDetector) Count(ctx context.Context, path string, _ float32) (map[string]int, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.err != nil {
		return nil, f.err
	}
	f.paths = append(f.paths, path)
	people := 2
	if strings.Contains(filepath.Base(path), "empty") {
		people = 0
	}
	return map[string]int{"person": people, "car": 0}, nil
}

func testDetector(t *testing.T) (*Detector, Resolver, *fakeDetector) {
	t.Helper()
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.mp4")
	if err := os.WriteFile(sourcePath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BQUERY_DETECTION_MODEL", "")
	t.Setenv("BQUERY_DETECTION_CONFIDENCE", "")
	decoder := NewDecoder(dir)
	decoder.ffmpeg, decoder.ffprobe = "unused", "unused"
	d := NewDetector(decoder)
	fake := &fakeDetector{}
	d.open = func(context.Context, string, string) (detectionBackend, error) { return fake, nil }
	t.Cleanup(func() { d.Close() })
	stat, _ := os.Stat(sourcePath)
	source := Source{Key: "test", Path: sourcePath, Size: stat.Size(), Modified: stat.ModTime()}
	refs := map[string]Reference{}
	for _, id := range []string{"people", "empty", "other"} {
		if err := os.WriteFile(decoder.Path(id, Detection), []byte("cached analysis image"), 0600); err != nil {
			t.Fatal(err)
		}
		refs[id] = Reference{ID: id, Source: source, Frame: format.Frame{TrackID: 1}}
	}
	resolve := func(value any) (Reference, error) {
		id, ok := value.(string)
		if !ok {
			return Reference{}, fmt.Errorf("expected frame, got %T", value)
		}
		ref, ok := refs[id]
		if !ok {
			return Reference{}, fmt.Errorf("unknown frame")
		}
		return ref, nil
	}
	return d, resolve, fake
}

func TestObjectDetectionQueryAndCache(t *testing.T) {
	d, resolve, fake := testDetector(t)
	functions := engine.Functions{"person_count": PersonCount(d, resolve), "object_count": ObjectCount(d, resolve)}
	q, err := engine.Parse("SELECT frame, person_count(frame) AS people, object_count(frame, 'CAR') AS cars FROM frames WHERE person_count(frame) > 0")
	if err != nil {
		t.Fatal(err)
	}
	tables := map[string]format.Table{"frames": {Columns: []string{"frame"}, Rows: []format.Row{{"frame": "people"}, {"frame": "empty"}, {"frame": "people"}, {"frame": nil}}}}
	r, err := q.ExecuteWith(context.Background(), tables, functions)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Rows) != 2 || r.Rows[0]["people"] != int64(2) || r.Rows[0]["cars"] != int64(0) {
		t.Fatalf("%+v", r)
	}
	if !reflect.DeepEqual(fake.paths, []string{d.decoder.Path("empty", Detection), d.decoder.Path("people", Detection)}) {
		t.Fatalf("duplicate inference: %v", fake.paths)
	}
	for _, id := range []string{"people", "empty"} {
		if _, err := os.Stat(d.decoder.Path(id, Detection)); !os.IsNotExist(err) {
			t.Fatal("analysis image retained after inference")
		}
	}
	if _, err := PersonCount(d, resolve).Evaluate(context.Background(), [][]any{{"other"}}); err != nil {
		t.Fatal(err)
	}
	if d.backend != fake {
		t.Fatal("model was reloaded")
	}
	if _, err := ObjectCount(d, resolve).Evaluate(context.Background(), [][]any{{"people", "typo"}}); err == nil || !strings.Contains(err.Error(), "unknown object class") {
		t.Fatal(err)
	}
	ref, _ := resolve("people")
	if err := os.WriteFile(ref.Source.Path, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := PersonCount(d, resolve).Evaluate(context.Background(), [][]any{{"people"}}); err == nil || !strings.Contains(err.Error(), "source changed") {
		t.Fatal(err)
	}
}

func TestObjectDetectionNullsTypesAndConfiguration(t *testing.T) {
	d, resolve, _ := testDetector(t)
	d.open = func(context.Context, string, string) (detectionBackend, error) {
		t.Fatal("nulls initialized detection")
		return nil, nil
	}
	got, err := ObjectCount(d, resolve).Evaluate(context.Background(), [][]any{{nil, "person"}, {"invalid", nil}})
	if err != nil || !reflect.DeepEqual(got, []any{nil, nil}) {
		t.Fatalf("%v %v", got, err)
	}
	for _, args := range [][][]any{{{42, "person"}}, {{"people", 42}}, {{"people", " "}}, {{"people"}}} {
		if _, err := ObjectCount(d, resolve).Evaluate(context.Background(), args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := PersonCount(d, resolve).Evaluate(ctx, [][]any{{nil}}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for _, confidence := range []string{"NaN", "Inf", "0", "1.01", "bad"} {
		d.confidence = confidence
		if _, err := PersonCount(d, resolve).Evaluate(context.Background(), [][]any{{"people"}}); err == nil || !strings.Contains(err.Error(), "CONFIDENCE") {
			t.Fatal(err)
		}
	}
}

func TestObjectDetectionInitializationRetryAndClose(t *testing.T) {
	d, resolve, fake := testDetector(t)
	attempts := 0
	d.open = func(_ context.Context, model, cacheDir string) (detectionBackend, error) {
		attempts++
		if model != "" {
			t.Fatal("default mode required a model setting")
		}
		if attempts == 1 {
			return nil, fmt.Errorf("download interrupted")
		}
		return fake, nil
	}
	if _, err := PersonCount(d, resolve).Evaluate(context.Background(), [][]any{{"people"}}); err == nil {
		t.Fatal("missing download error")
	}
	if _, err := PersonCount(d, resolve).Evaluate(context.Background(), [][]any{{"people"}}); err != nil {
		t.Fatal(err)
	}
	d.Close()
	if !fake.closed {
		t.Fatal("native resources were not closed")
	}
	if _, err := PersonCount(d, resolve).Evaluate(context.Background(), [][]any{{"people"}}); err == nil {
		t.Fatal("closed detector accepted work")
	}
}
