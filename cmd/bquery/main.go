package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"

	"binaryquery/engine"
	"binaryquery/format"
	"binaryquery/format/mp4"
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
		parsed, err := readSource(ctx, path, adapters)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		for name, t := range parsed {
			existing := tables[name]
			existing.Columns = t.Columns
			existing.Rows = append(existing.Rows, t.Rows...)
			tables[name] = existing
		}
	}
	result, err := q.Execute(ctx, tables)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(result)
}

func readSource(ctx context.Context, path string, adapters []format.Adapter) (map[string]format.Table, error) {
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
			return a.Read(ctx, s)
		}
	}
	return nil, fmt.Errorf("unsupported format (currently MP4/MOV)")
}
