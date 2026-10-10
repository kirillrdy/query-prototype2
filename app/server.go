// Package app serves a local query workbench and renders binary frame values.
package app

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"binaryquery/engine"
	"binaryquery/format"
	"binaryquery/format/mp4"
	"binaryquery/video"
)

//go:embed static/*
var static embed.FS

type FrameValue struct {
	format.Frame
	ID         string `json:"id"`
	PreviewURL string `json:"preview_url"`
	RawURL     string `json:"raw_url"`
}
type source struct {
	mu             sync.RWMutex
	indexGate      chan struct{}
	id, path, name string
	size           int64
	modified       time.Time
	tables         map[string]format.Table
	frames         map[string]format.Frame
}
type Server struct {
	mu       sync.RWMutex
	sources  []*source
	temp     string
	renderer *renderer
	handler  http.Handler
}

func New(ctx context.Context, paths []string) (*Server, error) {
	temp, err := os.MkdirTemp("", "binaryquery-web-")
	if err != nil {
		return nil, err
	}
	s := &Server{temp: temp, renderer: newRenderer(temp)}
	for _, path := range paths {
		if err := s.addSource(ctx, path, filepath.Base(path)); err != nil {
			s.Close()
			return nil, err
		}
	}
	assets, _ := fs.Sub(static, "static")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/info", s.info)
	mux.HandleFunc("POST /api/query", s.query)
	mux.HandleFunc("POST /api/query/stream", s.queryStream)
	mux.HandleFunc("POST /api/upload", s.upload)
	mux.HandleFunc("POST /api/source", s.openSource)
	mux.HandleFunc("DELETE /api/source/{id}", s.removeSource)
	mux.HandleFunc("POST /api/render", s.render)
	mux.HandleFunc("GET /api/frame/{id}/preview", s.preview)
	mux.HandleFunc("GET /api/frame/{id}/raw", s.raw)
	mux.Handle("GET /", http.FileServer(http.FS(assets)))
	// Only same-origin browser requests may mutate local state.
	s.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' blob:; style-src 'self'; script-src 'self'; object-src 'none'; frame-ancestors 'none'")
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if host != "127.0.0.1" && host != "localhost" && host != "::1" {
			fail(w, http.StatusForbidden, fmt.Errorf("use the localhost URL printed by the app"))
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+r.Host {
			fail(w, http.StatusForbidden, fmt.Errorf("cross-origin requests are not allowed"))
			return
		}
		if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			fail(w, http.StatusForbidden, fmt.Errorf("cross-site requests are not allowed"))
			return
		}
		mux.ServeHTTP(w, r)
	})
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }
func (s *Server) Close() error                                     { return os.RemoveAll(s.temp) }

func (s *Server) addSource(ctx context.Context, path, name string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	f, err := os.Open(abs)
	if err != nil {
		return err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return err
	}
	if !stat.Mode().IsRegular() {
		return fmt.Errorf("input must be a regular file")
	}
	input := format.Source{Name: name, Size: stat.Size(), Reader: f}
	a := mp4.Adapter{}
	if !a.Probe(input) {
		return fmt.Errorf("unsupported format: select an MP4 or MOV file")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	src := &source{id: hex.EncodeToString(random[:]), path: abs, name: name, size: stat.Size(), modified: stat.ModTime(), tables: map[string]format.Table{}, frames: map[string]format.Frame{}, indexGate: make(chan struct{}, 1)}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sources) >= 16 {
		return fmt.Errorf("at most 16 sources can be loaded per session")
	}
	s.sources = append(s.sources, src)
	return nil
}

func (s *source) unchanged() error {
	stat, err := os.Stat(s.path)
	if err != nil {
		return err
	}
	if stat.Size() != s.size || !stat.ModTime().Equal(s.modified) {
		return fmt.Errorf("source changed since loading; restart or upload it again")
	}
	return nil
}

func (s *Server) info(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	type item struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Size   int64  `json:"size"`
		Frames *int   `json:"frames"`
	}
	items := make([]item, 0, len(s.sources))
	for _, src := range s.sources {
		src.mu.RLock()
		var count *int
		if frames, ok := src.tables["frames"]; ok {
			n := frames.Len()
			count = &n
		}
		src.mu.RUnlock()
		items = append(items, item{src.id, src.name, src.size, count})
	}
	schemas := map[string][]string{}
	for name, t := range mp4.Schemas() {
		schemas[name] = t.Columns
	}
	writeJSON(w, map[string]any{"sources": items, "schemas": schemas, "decoder_available": s.renderer.available()})
}

func (s *Server) removeSource(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, src := range s.sources {
		if src.id == r.PathValue("id") {
			copy(s.sources[i:], s.sources[i+1:])
			s.sources[len(s.sources)-1] = nil
			s.sources = s.sources[:len(s.sources)-1]
			// Only remove the source from the session. Original files are never
			// deleted; temporary uploads and cached images expire on Close.
			writeJSON(w, map[string]bool{"removed": true})
			return
		}
	}
	fail(w, 404, fmt.Errorf("unknown source"))
}

func (s *Server) query(w http.ResponseWriter, r *http.Request)       { s.executeQuery(w, r, false) }
func (s *Server) queryStream(w http.ResponseWriter, r *http.Request) { s.executeQuery(w, r, true) }
func (s *Server) executeQuery(w http.ResponseWriter, r *http.Request, streaming bool) {
	var request struct {
		SQL string `json:"sql"`
	}
	if err := readJSON(w, r, &request); err != nil {
		fail(w, 400, err)
		return
	}
	q, err := engine.Parse(request.SQL)
	if err != nil {
		fail(w, 400, err)
		return
	}
	s.mu.RLock()
	sources := append([]*source(nil), s.sources...)
	s.mu.RUnlock()
	if len(sources) == 0 {
		fail(w, 400, fmt.Errorf("load a video first"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	tableName := q.SourceTable()
	if _, ok := mp4.Schemas()[tableName]; !ok {
		fail(w, 400, fmt.Errorf("unknown table %q", tableName))
		return
	}
	tables := map[string]format.Table{}
	for _, src := range sources {
		if err := src.unchanged(); err != nil {
			fail(w, 409, err)
			return
		}
		t, err := src.loadTable(ctx, tableName)
		if err != nil {
			fail(w, 400, fmt.Errorf("%s: %w", src.name, err))
			return
		}
		tables[tableName] = format.Append(tables[tableName], t)
	}
	functions := engine.Functions{"motion": video.Motion(s.renderer.decoder, s.resolveFrame)}
	if streaming {
		s.streamQuery(ctx, w, q, tables, functions)
		return
	}
	result, err := q.ExecuteWith(ctx, tables, functions)
	if err != nil {
		fail(w, 400, err)
		return
	}
	writeJSON(w, struct {
		engine.Result
		Truncated bool `json:"truncated"`
	}{result, false})
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	reader, err := r.MultipartReader()
	if err != nil {
		fail(w, 400, err)
		return
	}
	part, err := reader.NextPart()
	if err != nil {
		fail(w, 400, fmt.Errorf("choose a file"))
		return
	}
	defer part.Close()
	if part.FormName() != "file" || part.FileName() == "" {
		fail(w, 400, fmt.Errorf("expected one file upload"))
		return
	}
	f, err := os.CreateTemp(s.temp, "source-*.mp4")
	if err != nil {
		fail(w, 500, err)
		return
	}
	path := f.Name()
	retained := false
	defer func() {
		if !retained {
			os.Remove(path)
		}
	}()
	_, copyErr := io.Copy(f, part)
	closeErr := f.Close()
	if copyErr != nil {
		fail(w, 400, copyErr)
		return
	}
	if closeErr != nil {
		fail(w, 500, closeErr)
		return
	}
	if _, err := reader.NextPart(); err != io.EOF {
		fail(w, 400, fmt.Errorf("upload one file at a time"))
		return
	}
	if err := s.addSource(r.Context(), path, filepath.Base(part.FileName())); err != nil {
		fail(w, 400, err)
		return
	}
	retained = true
	writeJSON(w, map[string]bool{"loaded": true})
}

func (s *Server) lookup(id string) (*source, format.Frame, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, src := range s.sources {
		src.mu.RLock()
		frame, ok := src.frames[id]
		src.mu.RUnlock()
		if ok {
			return src, frame, nil
		}
	}
	return nil, format.Frame{}, fmt.Errorf("unknown frame")
}

func (s *Server) raw(w http.ResponseWriter, r *http.Request) {
	src, frame, err := s.lookup(r.PathValue("id"))
	if err != nil {
		fail(w, 404, err)
		return
	}
	if err := src.unchanged(); err != nil {
		fail(w, 409, err)
		return
	}
	f, err := os.Open(src.path)
	if err != nil {
		fail(w, 500, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"track-%d-sample-%d.bin\"", frame.TrackID, frame.SampleIndex))
	http.ServeContent(w, r, "frame.bin", src.modified, io.NewSectionReader(f, frame.Offset, int64(frame.Size)))
}

func (s *Server) render(w http.ResponseWriter, r *http.Request) {
	var request struct {
		IDs []string `json:"ids"`
	}
	if err := readJSON(w, r, &request); err != nil {
		fail(w, 400, err)
		return
	}
	if len(request.IDs) == 0 || len(request.IDs) > video.MaxFrames {
		fail(w, 400, fmt.Errorf("select between 1 and %d frames per render batch", video.MaxFrames))
		return
	}
	groups := map[*source]map[uint32]map[string]format.Frame{}
	for _, id := range request.IDs {
		src, frame, err := s.lookup(id)
		if err != nil {
			fail(w, 404, err)
			return
		}
		if groups[src] == nil {
			groups[src] = map[uint32]map[string]format.Frame{}
		}
		if groups[src][frame.TrackID] == nil {
			groups[src][frame.TrackID] = map[string]format.Frame{}
		}
		groups[src][frame.TrackID][id] = frame
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	for src, tracks := range groups {
		for track, frames := range tracks {
			if err := s.renderer.prepare(ctx, src, track, frames); err != nil {
				fail(w, 422, err)
				return
			}
		}
	}
	writeJSON(w, map[string]bool{"ready": true})
}

func (s *Server) preview(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	src, _, err := s.lookup(id)
	if err != nil {
		fail(w, 404, err)
		return
	}
	if err := src.unchanged(); err != nil {
		fail(w, 409, err)
		return
	}
	path := filepath.Join(s.temp, id+".jpg")
	if _, err := os.Stat(path); err != nil {
		fail(w, 404, fmt.Errorf("preview has not been rendered"))
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	http.ServeFile(w, r, path)
}

func readJSON(w http.ResponseWriter, r *http.Request, value any) error {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return fmt.Errorf("expected application/json")
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	if err := d.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("expected one JSON object")
	}
	return nil
}
func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(value)
}
func fail(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

// loadTable serializes first indexing, permits canceled waiters, and only
// publishes a complete index. A canceled or failed query can be retried.
func (s *source) loadTable(ctx context.Context, name string) (format.Table, error) {
	select {
	case s.indexGate <- struct{}{}:
		defer func() { <-s.indexGate }()
	case <-ctx.Done():
		return format.Table{}, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return format.Table{}, err
	}
	s.mu.RLock()
	t, ok := s.tables[name]
	s.mu.RUnlock()
	if ok {
		return t, nil
	}
	f, err := os.Open(s.path)
	if err != nil {
		return format.Table{}, err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return format.Table{}, err
	}
	if stat.Size() != s.size || !stat.ModTime().Equal(s.modified) {
		return format.Table{}, fmt.Errorf("source changed since loading; open it again")
	}
	tables, err := (mp4.Adapter{}).ReadTables(ctx, format.Source{Name: s.name, Size: s.size, Reader: f}, name)
	if err != nil {
		return format.Table{}, err
	}
	if err := s.unchanged(); err != nil {
		return format.Table{}, err
	}
	if frames, ok := tables["frames"]; ok {
		// Only materialize identities when a query actually accesses frame values.
		tables["frames"] = format.Table{Columns: frames.Columns, Count: frames.Len(), ValueAt: func(i int, c string) any {
			value := frames.Value(i, c)
			if frame, ok := value.(format.Frame); ok {
				id := fmt.Sprintf("%s-%d-%d", s.id, frame.TrackID, frame.SampleIndex)
				s.mu.Lock()
				s.frames[id] = frame
				s.mu.Unlock()
				return FrameValue{frame, id, "/api/frame/" + id + "/preview", "/api/frame/" + id + "/raw"}
			}
			return value
		}}
	}
	s.mu.Lock()
	for key, t := range tables {
		if _, exists := s.tables[key]; !exists {
			s.tables[key] = t
		}
	}
	t = s.tables[name]
	s.mu.Unlock()
	return t, nil
}

func (s *Server) openSource(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Path string `json:"path"`
	}
	if err := readJSON(w, r, &input); err != nil {
		fail(w, 400, err)
		return
	}
	if strings.TrimSpace(input.Path) == "" {
		fail(w, 400, fmt.Errorf("enter a local file path"))
		return
	}
	if err := s.addSource(r.Context(), input.Path, filepath.Base(input.Path)); err != nil {
		fail(w, 400, err)
		return
	}
	writeJSON(w, map[string]bool{"loaded": true})
}

// NDJSON carries complete rows, so an error after output leaves an explicit
// partial result rather than silently turning an unfinished query into success.
func (s *Server) streamQuery(ctx context.Context, w http.ResponseWriter, q engine.Query, tables map[string]format.Table, functions engine.Functions) {
	started := false
	encoder := json.NewEncoder(w)
	controller := http.NewResponseController(w)
	send := func(event any) error {
		if !started {
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Accel-Buffering", "no")
			started = true
		}
		if err := encoder.Encode(event); err != nil {
			return err
		}
		return controller.Flush()
	}
	err := q.Stream(ctx, tables, functions, func(chunk engine.Result) error {
		if chunk.Rows == nil {
			return send(map[string]any{"type": "schema", "columns": chunk.Columns})
		}
		return send(map[string]any{"type": "rows", "rows": chunk.Rows})
	})
	if err != nil {
		if !started {
			fail(w, 400, err)
		} else if ctx.Err() != context.Canceled {
			send(map[string]any{"type": "error", "error": err.Error()})
		}
		return
	}
	send(map[string]any{"type": "done", "truncated": false})
}
