package video

import (
	"context"
	"fmt"
	"testing"
)

func TestDifference(t *testing.T) {
	for _, tc := range []struct {
		a, b []byte
		want float64
	}{
		{[]byte{0, 50, 255}, []byte{0, 50, 255}, 0}, {[]byte{0, 0}, []byte{255, 255}, 1}, {[]byte{0, 255}, []byte{255, 255}, 0.5},
	} {
		v, err := difference(tc.a, tc.b)
		if err != nil || v != tc.want {
			t.Fatalf("difference=%g error=%v", v, err)
		}
	}
	if _, err := difference(nil, nil); err == nil {
		t.Fatal("empty pixels accepted")
	}
	if _, err := difference([]byte{1}, []byte{1, 2}); err == nil {
		t.Fatal("different dimensions accepted")
	}
}
func TestMotionNullsAndTypes(t *testing.T) {
	resolve := func(value any) (Reference, error) { return Reference{}, fmt.Errorf("expected frame, got %T", value) }
	function := Motion(&Decoder{}, resolve)
	r, err := function.Evaluate(context.Background(), [][]any{{nil, nil}, {"invalid", nil}})
	if err != nil || len(r) != 2 || r[0] != nil || r[1] != nil {
		t.Fatalf("%v %v", r, err)
	}
	if _, err := function.Evaluate(context.Background(), [][]any{{1, 2}}); err == nil {
		t.Fatal("scalar arguments accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := function.Evaluate(ctx, [][]any{{nil, nil}}); err != context.Canceled {
		t.Fatal(err)
	}
}
