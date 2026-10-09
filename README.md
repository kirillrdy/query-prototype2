# Binary Query

A Go query engine that exposes binary files as tables. The first adapter reads
MP4/MOV container metadata and encoded video-frame timing using `io.ReaderAt`, without FFmpeg or video
decoding. The query engine depends on the adapter interface, not the file format.

## Run

Requires Go 1.26 or newer. No external Go dependencies.

```sh
go build -o bquery ./cmd/bquery
./bquery -query "SELECT sample_index, pts_seconds, dts_seconds, size FROM frames WHERE pts_seconds >= 0.5 LIMIT 10" clip.mp4
./bquery -query "SELECT path, sample_index, pts_seconds FROM frames WHERE is_keyframe = true" clip.mp4
./bquery -query "SELECT path, codec, width, height FROM tracks WHERE handler = 'vide' AND width >= 1920" clip.mp4
./bquery -query "SELECT path, duration_seconds, track_count FROM files" clip.mp4 other.mov
./bquery -query "SELECT box_path, offset, size FROM boxes WHERE type = 'mdat' LIMIT 10" clip.mp4
go test ./...
```

Flags must precede file paths. Results are JSON with an ordered `columns` array
and a `rows` array. Queries span all supplied files; LIMIT applies globally.
Any input error fails the command before results are emitted.

## Tables

| Table | Columns |
| --- | --- |
| `files` | `path`, `format`, `size`, `major_brand`, `duration_seconds`, `track_count` |
| `tracks` | `path`, `track_id`, `handler`, `codec`, `width`, `height`, `timescale`, `duration_seconds` |
| `boxes` | `path`, `box_path`, `type`, `offset`, `size`, `depth` |
| `frames` | `path`, `track_id`, `sample_index`, `dts_seconds`, `pts_seconds`, `duration_seconds`, `size`, `is_keyframe` |

Sizes and offsets are bytes; dimensions are track-header 16.16 fixed-point
values, converted to pixels; durations are seconds. Handlers are raw four-byte
codes (`vide`, `soun`); codecs are the first sample-entry code (`avc1`, `hvc1`,
`mp4a`, etc.). Missing metadata is JSON `null`. Box paths describe ancestry and
can repeat; offsets identify individual boxes. Both MP4 and MOV report format
`mp4` because they share this structural adapter.

`frames` contains video samples only, in decode order. Sample indices start at
1 within each track. DTS is decode time; PTS adds the composition offset for
reordered frames. Timestamps use the media timeline with DTS starting at zero;
movie edit lists are not applied. `duration_seconds` is the sample's decode
duration. When the sync table is absent, all samples are sync samples, as defined
by the format. The default query returns the first 100 frame rows.

## Query language

```text
SELECT * | column [, column ...]
FROM files | tracks | boxes | frames
[WHERE column operator literal [AND column operator literal ...]]
[LIMIT nonnegative_integer]
```

Keywords and identifiers are case insensitive. Operators: `=`, `!=`, `<>`, `<`,
`<=`, `>`, `>=`. Literals are numbers, `true`/`false`, or single-quoted strings (escape a quote
as `''`). Comparisons are type-sensitive; null values never match. A trailing
semicolon is accepted. No joins, aggregation, OR, aliases, ordering, or SQL
functions yet. Row order follows input order and box traversal order.

## Architecture and boundaries

- `format`: source, schema, row, and adapter interfaces.
- `format/mp4`: bounded box traversal, headers, sample descriptions, and sample timing.
- `engine`: query parsing, schema validation, filtering, projection, and limit.
- `cmd/bquery`: local-file adapter selection and JSON output.

The parser supports 32-bit sizes, extended 64-bit sizes, boxes extending to the
end of their parent, UUID headers, and version 0/1 timing and track headers. It
rejects invalid ranges and truncated metadata, caps nesting at 32 levels and
box count at 100,000 per file, sample-table counts and expanded frame count at
1,000,000, and checks cancellation during traversal. Unknown
boxes are opaque; known structural containers are traversed. Media payloads
are skipped with offsets, so memory use depends on metadata, not video size.
Metadata and frame rows are currently materialized before querying; LIMIT does not stop
file parsing, and there is no column or predicate pushdown yet.

This is a metadata prototype, not a complete MP4 validator. Fragment boxes are
listed, but fragment-derived duration and frames, sample byte offsets, compact
sample sizes (`stz2`), multiple sample
descriptions, edit-list timing, rotation, and compressed QuickTime movie headers
are not interpreted. Frame timing uses `stts`, `ctts` (including signed version
1 offsets), `stsz`, and `stss`; fragments do not contribute frame rows.
Durations reflect declared headers, not decoded playback;
unknown declared durations remain null. There is no pixel decoding or semantic
search of video contents. Format detection uses common leading box types,
followed by structural validation and a required movie box.

To add a format, implement `format.Adapter` and register it in the CLI. Future
work: sample-to-chunk mapping for byte-range queries, fragmented video support, streaming scans and
pushdown, then additional format adapters.

Format references: [Apple QuickTime atoms](https://developer.apple.com/documentation/quicktime-file-format/atoms),
[track header](https://developer.apple.com/documentation/quicktime-file-format/track_header_atom),
[media header](https://developer.apple.com/documentation/quicktime-file-format/media_header_atom),
[composition offsets](https://developer.apple.com/documentation/quicktime-file-format/composition_offset_atom).
