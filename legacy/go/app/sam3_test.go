package app

import (
	"encoding/json"
	"os"
	"testing"
)

func TestSAM3FrameIntegration(t *testing.T) {
	path := os.Getenv("BQUERY_TEST_SAM3_VIDEO")
	if path == "" {
		t.Skip("set BQUERY_TEST_SAM3_VIDEO and BQUERY_SAM3_WORKER for a real frame query")
	}
	s := newTestServer(t, path)
	w := request(s, "POST", "/api/query", []byte(`{"sql":"SELECT frame, sam3_count(frame, 'person') AS count, sam3_score(frame, 'person') AS score, sam3(frame, 'person') AS masks FROM frames WHERE is_keyframe = true LIMIT 1"}`))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var result struct {
		Rows []struct {
			Count int64   `json:"count"`
			Score float64 `json:"score"`
			Masks struct {
				Count  int64 `json:"count"`
				Width  int   `json:"width"`
				Height int   `json:"height"`
			} `json:"masks"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 1 || result.Rows[0].Masks.Width <= 0 || result.Rows[0].Masks.Height <= 0 || result.Rows[0].Count != result.Rows[0].Masks.Count {
		t.Fatalf("%+v", result)
	}
	t.Logf("frame query: %d masks, score %.3f", result.Rows[0].Count, result.Rows[0].Score)
}
