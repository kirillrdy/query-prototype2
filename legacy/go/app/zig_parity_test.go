package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func zigBinary(t *testing.T, name string) string {
	t.Helper()
	if path := os.Getenv("BQUERY_ZIG_WEB"); path != "" {
		return path
	}
	for _, prefix := range []string{"../zig-out/bin", "../../../zig-out/bin"} {
		path, _ := filepath.Abs(filepath.Join(prefix, name))
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	t.Skip("build the Zig binaries to run native web parity")
	return ""
}
func startZig(t *testing.T, paths ...string) string {
	t.Helper()
	cmd := exec.Command(zigBinary(t, "bquery-web"), append([]string{"-port=0"}, paths...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Signal(os.Interrupt)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			cmd.Process.Kill()
			<-done
		}
		if stderr.Len() > 0 {
			t.Logf("native server stderr: %s", stderr.String())
		}
	})
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() {
		t.Fatalf("native server did not start: %s", stderr.String())
	}
	return strings.TrimPrefix(scanner.Text(), "Binary Query: ")
}
func zigRequest(t *testing.T, base, method, path string, data []byte, contentType string) (int, []byte) {
	t.Helper()
	r, err := http.NewRequest(method, base+path, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	c := http.Client{Timeout: 20 * time.Second}
	response, err := c.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, body
}
func TestZigWebParity(t *testing.T) {
	path := emptyMovie(t)
	base := startZig(t, path)
	for _, asset := range []string{"/", "/app.js", "/graph.js", "/masks.js", "/style.css"} {
		code, body := zigRequest(t, base, "GET", asset, nil, "")
		if code != 200 || len(body) == 0 {
			t.Fatalf("%s: %d %s", asset, code, body)
		}
	}
	code, body := zigRequest(t, base, "GET", "/api/info", nil, "")
	if code != 200 || !bytes.Contains(body, []byte(`"frames":null`)) {
		t.Fatalf("lazy info: %d %s", code, body)
	}
	for _, endpoint := range []string{"/api/query", "/api/query/stream"} {
		code, body = zigRequest(t, base, "POST", endpoint, []byte(`{"sql":"SELECT sam3(null, 'person') AS masks, person_count(null) AS people, path FROM files"}`), "application/json")
		if code != 200 || !bytes.Contains(body, []byte(`"masks":null`)) || !bytes.Contains(body, []byte(`"people":null`)) {
			t.Fatalf("%s: %d %s", endpoint, code, body)
		}
		if endpoint == "/api/query/stream" && (!bytes.Contains(body, []byte(`"type":"schema"`)) || !bytes.Contains(body, []byte(`"type":"rows"`)) || !bytes.Contains(body, []byte(`"type":"done"`))) {
			t.Fatal(string(body))
		}
	}
	r, _ := http.NewRequest("POST", base+"/api/source", strings.NewReader(`{"path":"unused"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "http://evil.example")
	response, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 403 {
		t.Fatal("accepted cross-origin request")
	}
	var upload bytes.Buffer
	writer := multipart.NewWriter(&upload)
	part, _ := writer.CreateFormFile("file", "uploaded.mp4")
	data, _ := os.ReadFile(path)
	part.Write(data)
	writer.Close()
	code, body = zigRequest(t, base, "POST", "/api/upload", upload.Bytes(), writer.FormDataContentType())
	if code != 200 {
		t.Fatalf("upload: %d %s", code, body)
	}
	code, body = zigRequest(t, base, "GET", "/api/info", nil, "")
	var info struct {
		Sources []struct {
			ID   string
			Name string
		}
	}
	if err := json.Unmarshal(body, &info); err != nil {
		t.Fatal(err)
	}
	if len(info.Sources) != 2 || info.Sources[1].Name != "uploaded.mp4" {
		t.Fatal(string(body))
	}
	code, body = zigRequest(t, base, "DELETE", "/api/source/"+info.Sources[0].ID, nil, "")
	if code != 200 {
		t.Fatal(string(body))
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("removed original source")
	}
}
func TestZigRenderedFramesAndMotion(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg required")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe required")
	}
	path := filepath.Join(t.TempDir(), "reordered.mp4")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := command(ctx, ffmpeg, "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=160x96:rate=6", "-t", "2", "-c:v", "mpeg4", "-bf", "2", path); err != nil {
		t.Fatal(err)
	}
	base := startZig(t, path)
	s := newTestServer(t, path)
	sql := []byte(`{"sql":"SELECT frame, sample_index, pts_seconds, motion(frame, LAG(frame) OVER (ORDER BY pts_seconds, sample_index)) AS motion_score FROM frames WHERE sample_index BETWEEN 2 AND 5 ORDER BY pts_seconds"}`)
	expected := request(s, "POST", "/api/query", sql)
	if expected.Code != 200 {
		t.Fatal(expected.Body.String())
	}
	code, body := zigRequest(t, base, "POST", "/api/query", sql, "application/json")
	if code != 200 {
		t.Fatalf("query: %d %s", code, body)
	}
	type row struct {
		Frame FrameValue `json:"frame"`
		Score *float64   `json:"motion_score"`
		Index int        `json:"sample_index"`
	}
	type result struct{ Rows []row }
	var want, got result
	json.Unmarshal(expected.Body.Bytes(), &want)
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Rows) != 4 {
		t.Fatal(string(body))
	}
	ids := []string{}
	for i, row := range got.Rows {
		w := want.Rows[i]
		if row.Frame.Offset != w.Frame.Offset || row.Index != w.Index || (row.Score == nil) != (w.Score == nil) || (row.Score != nil && *row.Score != *w.Score) {
			t.Fatalf("frame/motion mismatch: %+v %+v", row, w)
		}
		ids = append(ids, row.Frame.ID)
	}
	requestBody, _ := json.Marshal(map[string]any{"ids": ids})
	code, body = zigRequest(t, base, "POST", "/api/render", requestBody, "application/json")
	if code != 200 {
		t.Fatalf("render: %d %s", code, body)
	}
	goIDs := []string{}
	for _, row := range want.Rows {
		goIDs = append(goIDs, row.Frame.ID)
	}
	goBody, _ := json.Marshal(map[string]any{"ids": goIDs})
	rendered := request(s, "POST", "/api/render", goBody)
	if rendered.Code != 200 {
		t.Fatal(rendered.Body.String())
	}
	for i, row := range got.Rows {
		code, body = zigRequest(t, base, "GET", row.Frame.PreviewURL, nil, "")
		w := request(s, "GET", want.Rows[i].Frame.PreviewURL, nil)
		if code != 200 || w.Code != 200 || !bytes.Equal(body, w.Body.Bytes()) {
			t.Fatalf("preview differs for sample %d", row.Index)
		}
		code, body = zigRequest(t, base, "GET", row.Frame.RawURL, nil, "")
		w = request(s, "GET", want.Rows[i].Frame.RawURL, nil)
		if code != 200 || !bytes.Equal(body, w.Body.Bytes()) {
			t.Fatalf("raw sample differs: %d", row.Index)
		}
		r, _ := http.NewRequest("GET", base+row.Frame.RawURL, nil)
		r.Header.Set("Range", "bytes=1-3")
		response, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		partial, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || response.StatusCode != 206 || !bytes.Equal(partial, body[1:4]) || response.Header.Get("Content-Disposition") == "" {
			t.Fatalf("raw byte range: %d %v %q", response.StatusCode, err, partial)
		}
	}
}
