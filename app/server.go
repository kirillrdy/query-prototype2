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
)

//go:embed static/*
var static embed.FS

const maxResultRows = 500
const maxUploadBytes = 1 << 30

type FrameValue struct {
	format.Frame
	ID         string `json:"id"`
	PreviewURL string `json:"preview_url"`
	RawURL     string `json:"raw_url"`
}
type source struct {
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
	mux.HandleFunc("POST /api/upload", s.upload)
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
	tables, err := a.Read(ctx, input)
	if err != nil {
		return err
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	src := &source{id: hex.EncodeToString(random[:]), path: abs, name: name, size: stat.Size(), modified: stat.ModTime(), tables: tables, frames: map[string]format.Frame{}}
	for _, row := range tables["frames"].Rows {
		if frame, ok := row["frame"].(format.Frame); ok {
			id := fmt.Sprintf("%s-%d-%d", src.id, frame.TrackID, frame.SampleIndex)
			src.frames[id] = frame
			row["frame"] = FrameValue{frame, id, "/api/frame/" + id + "/preview", "/api/frame/" + id + "/raw"}
		}
	}
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
		Frames int    `json:"frames"`
	}
	items := make([]item, 0, len(s.sources))
	for _, src := range s.sources {
		items = append(items, item{src.id, src.name, src.size, len(src.tables["frames"].Rows)})
	}
	schemas := map[string][]string{}
	if len(s.sources) > 0 {
		for name, t := range s.sources[0].tables {
			schemas[name] = t.Columns
		}
	}
	writeJSON(w, map[string]any{"sources": items, "schemas": schemas, "decoder_available": s.renderer.available(), "max_rows": maxResultRows})
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

func (s *Server) query(w http.ResponseWriter, r *http.Request) {
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
	tables := map[string]format.Table{}
	for _, src := range sources {
		if err := src.unchanged(); err != nil {
			fail(w, 409, err)
			return
		}
		for name, t := range src.tables {
			merged := tables[name]
			merged.Columns = t.Columns
			merged.Rows = append(merged.Rows, t.Rows...)
			tables[name] = merged
		}
	}
	result, err := q.WithMaxRows(maxResultRows+1).Execute(r.Context(), tables)
	if err != nil {
		fail(w, 400, err)
		return
	}
	truncated := len(result.Rows) > maxResultRows
	if truncated {
		result.Rows = result.Rows[:maxResultRows]
	}
	writeJSON(w, struct {
		engine.Result
		Truncated bool `json:"truncated"`
	}{result, truncated})
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
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
		if frame, ok := src.frames[id]; ok {
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
	if len(request.IDs) == 0 || len(request.IDs) > maxResultRows {
		fail(w, 400, fmt.Errorf("select between 1 and %d frames", maxResultRows))
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
