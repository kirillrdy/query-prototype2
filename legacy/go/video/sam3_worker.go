package video

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type sam3Worker struct {
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    *bufio.Scanner
	stderr *boundedBuffer
	closed bool
}

func openSAM3Backend() (sam3Backend, error) {
	path := os.Getenv("BQUERY_SAM3_WORKER")
	if path == "" {
		path, _ = exec.LookPath("bquery-sam3")
		if path == "" {
			if exe, err := os.Executable(); err == nil {
				candidate := filepath.Join(filepath.Dir(exe), "bquery-sam3")
				if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
					path = candidate
				}
			}
		}
	}
	if path == "" {
		return nil, fmt.Errorf("SAM3 requires bquery-sam3: build sam3-worker with zig build -Doptimize=ReleaseFast and set BQUERY_SAM3_WORKER to its executable")
	}
	w := &sam3Worker{cmd: exec.Command(path), stderr: &boundedBuffer{limit: 1 << 20}}
	w.cmd.Stderr = w.stderr
	var err error
	w.in, err = w.cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := w.cmd.StdoutPipe()
	if err != nil {
		w.in.Close()
		return nil, err
	}
	w.out = bufio.NewScanner(out)
	w.out.Buffer(make([]byte, 4096), 32<<20)
	if err := w.cmd.Start(); err != nil {
		w.in.Close()
		out.Close()
		return nil, fmt.Errorf("start SAM3 worker: %w", err)
	}
	return w, nil
}

func (w *sam3Worker) Close() {
	if w.closed {
		return
	}
	w.closed = true
	w.in.Close()
	w.cmd.Process.Kill()
	w.cmd.Wait()
}

func (w *sam3Worker) Find(ctx context.Context, path, phrase string, confidence float32) (SAM3Result, error) {
	if err := ctx.Err(); err != nil {
		return SAM3Result{}, err
	}
	if w.closed {
		return SAM3Result{}, fmt.Errorf("SAM3 worker is closed")
	}
	type response struct {
		result SAM3Result
		err    error
	}
	done := make(chan response, 1)
	go func() {
		err := json.NewEncoder(w.in).Encode(struct {
			Path     string  `json:"path"`
			Phrase   string  `json:"phrase"`
			MinScore float32 `json:"min_score"`
		}{path, phrase, confidence})
		var r struct {
			SAM3Result
			Error string `json:"err"`
		}
		if err == nil {
			if !w.out.Scan() {
				err = w.out.Err()
				if err == nil {
					err = io.ErrUnexpectedEOF
				}
			} else {
				err = json.Unmarshal(w.out.Bytes(), &r)
			}
		}
		if err == nil && r.Error != "" {
			err = fmt.Errorf("%s", r.Error)
		}
		done <- response{r.SAM3Result, err}
	}()
	select {
	case <-ctx.Done():
		w.Close()
		<-done
		return SAM3Result{}, ctx.Err()
	case r := <-done:
		if r.err != nil {
			w.Close()
			message := strings.TrimSpace(w.stderr.String())
			if len(message) > 2000 {
				message = message[len(message)-2000:]
			}
			return SAM3Result{}, fmt.Errorf("SAM3 inference: %w %s", r.err, message)
		}
		return r.result, nil
	}
}
