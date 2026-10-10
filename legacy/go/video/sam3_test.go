package video

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"binaryquery/engine"
	"binaryquery/format"
)

type fakeSAM3 struct {
	calls  int
	closed bool
	err    error
}

func (f *fakeSAM3) Close() { f.closed = true }
func (f *fakeSAM3) Find(ctx context.Context, path, phrase string, threshold float32) (SAM3Result, error) {
	if f.err != nil {
		return SAM3Result{}, f.err
	}
	f.calls++
	count := int64(2)
	if phrase == "empty" {
		count = 0
	}
	return SAM3Result{Count: count, Score: 0.8, Width: 2, Height: 2, Masks: []SAM3Mask{{Score: 0.8, RLE: []uint32{1, 3}}}}, nil
}

func testSegmenter(t *testing.T) (*Segmenter, Resolver, *fakeSAM3) {
	t.Helper()
	d, resolve, _ := testDetector(t)
	for _, id := range []string{"people", "empty", "other"} {
		if err := os.WriteFile(d.decoder.Path(id, Segmentation), []byte("analysis image"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("BQUERY_SAM3_CONFIDENCE", "")
	s := NewSegmenter(d.decoder)
	f := &fakeSAM3{}
	s.open = func() (sam3Backend, error) { return f, nil }
	t.Cleanup(func() { s.Close() })
	return s, resolve, f
}

func TestSAM3QueryCacheAndCleanup(t *testing.T) {
	s, resolve, fake := testSegmenter(t)
	q, err := engine.Parse("SELECT sam3(frame, 'person') AS masks, sam3_score(frame, 'person') AS score, sam3_count(frame, 'person') AS count FROM frames WHERE sam3_count(frame, 'person') > 0 LIMIT 2")
	if err != nil {
		t.Fatal(err)
	}
	tables := map[string]format.Table{"frames": {Columns: []string{"frame"}, Rows: []format.Row{{"frame": "people"}, {"frame": "people"}, {"frame": nil}}}}
	r, err := q.ExecuteWith(context.Background(), tables, engine.Functions{"sam3": SAM3(s, resolve), "sam3_count": SAM3Count(s, resolve), "sam3_score": SAM3Score(s, resolve)})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Rows) != 2 || r.Rows[0]["count"] != int64(2) || r.Rows[0]["score"] != 0.8 || r.Rows[0]["masks"].(SAM3Result).Count != 2 {
		t.Fatalf("%+v", r)
	}
	if fake.calls != 1 {
		t.Fatalf("duplicate inference: %d calls", fake.calls)
	}
	if _, err := os.Stat(s.decoder.Path("people", Segmentation)); !os.IsNotExist(err) {
		t.Fatal("segmentation image was retained")
	}
}

func TestSAM3NullValidationCloseAndRetry(t *testing.T) {
	s, resolve, fake := testSegmenter(t)
	f := SAM3Count(s, resolve)
	out, err := f.Evaluate(context.Background(), [][]any{{nil, "person"}, {"people", nil}})
	if err != nil || out[0] != nil || out[1] != nil || s.backend != nil {
		t.Fatalf("nulls: %v %v", out, err)
	}
	for _, tuple := range [][]any{{"people"}, {"people", 42}, {"people", "  "}, {42, "person"}} {
		if _, err := f.Evaluate(context.Background(), [][]any{tuple}); err == nil {
			t.Fatalf("accepted %v", tuple)
		}
	}
	s.confidence = "NaN"
	if _, err := f.Evaluate(context.Background(), [][]any{{"people", "person"}}); err == nil {
		t.Fatal("accepted NaN")
	}
	s.confidence = "0.5"
	fake.err = errors.New("inference failed")
	if _, err := f.Evaluate(context.Background(), [][]any{{"people", "person"}}); err == nil || !fake.closed || s.backend != nil {
		t.Fatal("failed backend was not released")
	}
	// A failed call releases decoded files; supply another cached frame to retry.
	fake.err = nil
	if _, err := f.Evaluate(context.Background(), [][]any{{"other", "person"}}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := f.Evaluate(context.Background(), [][]any{{"other", "person"}}); err == nil {
		t.Fatal("accepted work after close")
	}
}

func TestSAM3WorkerProtocolAndCancellation(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("requires /bin/sh")
	}
	path := filepath.Join(t.TempDir(), "worker")
	script := "#!/bin/sh\nwhile IFS= read -r request; do\n printf '%s\\n' '{\"count\":1,\"score\":0.9,\"width\":2,\"height\":2,\"masks\":[{\"score\":0.9,\"rle\":[0,4]}]}'\ndone\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BQUERY_SAM3_WORKER", path)
	w, err := openSAM3Backend()
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		r, err := w.Find(context.Background(), "/tmp/image.png", "a person", 0.5)
		if err != nil || r.Count != 1 || len(r.Masks) != 1 {
			t.Fatalf("%+v %v", r, err)
		}
	}
	w.Close()
	if err := os.WriteFile(path, []byte("#!/bin/sh\nread request\nexec sleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	w, err = openSAM3Backend()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = w.Find(ctx, "/tmp/image.png", "person", 0.5)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
		t.Fatalf("cancellation: %v after %v", err, time.Since(start))
	}
}

func TestSAM3NativeIntegration(t *testing.T) {
	image := os.Getenv("BQUERY_TEST_SAM3_IMAGE")
	if image == "" {
		t.Skip("set BQUERY_TEST_SAM3_IMAGE and BQUERY_SAM3_WORKER to test native GPU inference")
	}
	w, err := openSAM3Backend()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	r, err := w.Find(ctx, image, "cat", 0.5)
	if err != nil {
		t.Fatal(err)
	}
	if r.Count < 1 || int64(len(r.Masks)) != r.Count || r.Score < 0.5 {
		t.Fatalf("expected cat masks: %+v", r)
	}
	for _, mask := range r.Masks {
		var total uint64
		for _, run := range mask.RLE {
			total += uint64(run)
		}
		if total != uint64(r.Width*r.Height) {
			t.Fatalf("mask has %d pixels, dimensions %dx%d", total, r.Width, r.Height)
		}
	}
	t.Logf("%d cat masks, score %.3f, %dx%d", r.Count, r.Score, r.Width, r.Height)
}
