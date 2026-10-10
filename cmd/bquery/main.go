package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"binaryquery/engine"
	"binaryquery/format"
	"binaryquery/format/mp4"
	"binaryquery/video"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "bquery:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, out, stderr io.Writer) error {
	flags := flag.NewFlagSet("bquery", flag.ContinueOnError)
	flags.SetOutput(stderr)
	sql := flags.String("query", "SELECT * FROM frames LIMIT 100", "SQL query over frames, files, tracks, or boxes")
	flags.Usage = func() { fmt.Fprintln(stderr, "Usage: bquery [-query SQL] FILE [FILE ...]"); flags.PrintDefaults() }
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if flags.NArg() == 0 {
		flags.Usage()
		return fmt.Errorf("at least one input file is required")
	}
	q, err := engine.Parse(*sql)
	if err != nil {
		return err
	}
	adapters := []format.Adapter{mp4.Adapter{}}
	tables := map[string]format.Table{}
	for _, path := range flags.Args() {
		parsed, err := readQuerySource(ctx, path, adapters, q.SourceTable())
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		for name, t := range parsed {
			tables[name] = format.Append(tables[name], t)
		}
	}
	result, err := execute(ctx, q, tables)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(result)
}

func execute(ctx context.Context, q engine.Query, tables map[string]format.Table) (engine.Result, error) {
	var decoder *video.Decoder
	var detector *video.Detector
	var temp string
	defer func() {
		if detector != nil {
			detector.Close()
		}
		if temp != "" {
			os.RemoveAll(temp)
		}
	}()
	sources := map[string]video.Source{}
	resolve := func(value any) (video.Reference, error) {
		frame, ok := value.(format.Frame)
		if !ok {
			return video.Reference{}, fmt.Errorf("expected frame values, got %T", value)
		}
		source, exists := sources[frame.Source]
		if !exists {
			path, err := filepath.Abs(frame.Source)
			if err != nil {
				return video.Reference{}, err
			}
			stat, err := os.Stat(path)
			if err != nil {
				return video.Reference{}, err
			}
			hash := sha256.Sum256([]byte(path))
			source = video.Source{Key: fmt.Sprintf("%x", hash[:16]), Path: path, Size: stat.Size(), Modified: stat.ModTime()}
			sources[frame.Source] = source
		}
		id := fmt.Sprintf("%s-%d-%d", source.Key, frame.TrackID, frame.SampleIndex)
		return video.Reference{ID: id, Source: source, Frame: frame}, nil
	}
	initialize := func() error {
		if decoder == nil {
			var err error
			temp, err = os.MkdirTemp("", "binaryquery-functions-")
			if err != nil {
				return err
			}
			decoder = video.NewDecoder(temp)
			detector = video.NewDetector(decoder)
		}
		return nil
	}
	functions := engine.Functions{"motion": {Arity: 2, Evaluate: func(callCtx context.Context, args [][]any) ([]any, error) {
		if err := initialize(); err != nil {
			return nil, err
		}
		return video.Motion(decoder, resolve).Evaluate(callCtx, args)
	}}}
	functions["person_count"] = engine.Function{Arity: 1, MaxBatchSize: video.DetectionBatchSize, Evaluate: func(callCtx context.Context, args [][]any) ([]any, error) {
		if err := initialize(); err != nil {
			return nil, err
		}
		return video.PersonCount(detector, resolve).Evaluate(callCtx, args)
	}}
	functions["object_count"] = engine.Function{Arity: 2, MaxBatchSize: video.DetectionBatchSize, Evaluate: func(callCtx context.Context, args [][]any) ([]any, error) {
		if err := initialize(); err != nil {
			return nil, err
		}
		return video.ObjectCount(detector, resolve).Evaluate(callCtx, args)
	}}
	var bounded context.Context
	var cancel context.CancelFunc
	if q.UsesFunction("person_count", "object_count") {
		bounded, cancel = context.WithCancel(ctx)
	} else {
		bounded, cancel = context.WithTimeout(ctx, 2*time.Minute)
	}
	defer cancel()
	return q.ExecuteWith(bounded, tables, functions)
}

func readSource(ctx context.Context, path string, adapters []format.Adapter) (map[string]format.Table, error) {
	return readQuerySource(ctx, path, adapters, "frames")
}
func readQuerySource(ctx context.Context, path string, adapters []format.Adapter, table string) (map[string]format.Table, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("input must be a regular file")
	}
	s := format.Source{Name: path, Size: info.Size(), Reader: f}
	for _, a := range adapters {
		if a.Probe(s) {
			if lazy, ok := a.(interface {
				ReadTables(context.Context, format.Source, string) (map[string]format.Table, error)
			}); ok {
				return lazy.ReadTables(ctx, s, table)
			}
			return a.Read(ctx, s)
		}
	}
	return nil, fmt.Errorf("unsupported format (currently MP4/MOV)")
}
