# Binary Query

A Go query engine that exposes binary files as tables. The first adapter reads
MP4/MOV container metadata, frame timing, and encoded frame byte ranges using
`io.ReaderAt`. The query engine depends on the adapter interface, not the file
format. A local web workbench displays scalar fields in tables, previews binary
frame values, and plays selected frames using an optional FFmpeg decoder.

## Web workbench

```sh
go run ./cmd/bquery-web
# Or preload local files without uploading a copy:
go run ./cmd/bquery-web video.mp4 other.mov
```

Open the printed `http://127.0.0.1:8080` URL. Use **Open video** to load files, then:

```sql
SELECT frame, path, sample_index, pts_seconds
FROM frames
WHERE pts_seconds >= 0.5 AND pts_seconds < 2
LIMIT 100
```

Text, numbers, and booleans appear in a table. Click a `frame` cell to inspect its
decoded picture or download its original encoded bytes. Check the rows you want
and press **Play selection**. Playback defaults to presentation order within
each source/track, skips gaps, and holds each frame for its sample duration.
You can switch to query result order, scrub the selection, change speed, or
step one frame at a time. Playback is an image sequence, with no audio.
Pure scalar queries display only a table; `SELECT frame FROM frames ...` works
without including any identifying scalar columns.

The **×** beside a source removes it from the session and from subsequent
queries. It never deletes the original video. All loaded sources participate
in queries; you can also use `WHERE path = 'video.mp4'` to select one source.

Previews require `ffmpeg` and `ffprobe` on PATH. Metadata queries and binary
downloads work without them. FFprobe maps decoded pictures to exact sample byte
positions, so B-frames are identified correctly; FFmpeg then decodes the source
with its required reference pictures and exports only the selected pictures.
The first preview indexes the decoded track and may take time for long videos.
Preview images are cached JPEGs, scaled to fit 1280 × 720 without enlarging.
The downloaded `.bin` is an encoded sample, which may require codec headers and
other frames to decode; it is not a standalone video file.

The server binds to localhost only; use `-port 8090` to choose another port.
Queries are capped at 500 result rows in the UI, with a visible truncation note;
the CLI has no UI row cap. The session accepts up to 16 files, with uploads
limited to 1 GiB per request. Uploads and preview caches live in a temporary
directory and are deleted when the app exits normally. A removed source's
temporary copy/cache remains until session cleanup. Stop the app with Ctrl+C.
Files passed on the command line are read in place; changes detected after
loading require restarting or uploading again. Decoder work is serialized and
has a two-minute timeout per render request.

## Run

Requires Go 1.26 or newer. No external Go dependencies.

```sh
go build -o bquery ./cmd/bquery
./bquery -query "SELECT frame, sample_index, pts_seconds FROM frames WHERE pts_seconds >= 0.5 LIMIT 10" clip.mp4
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
| `frames` | `path`, `track_id`, `sample_index`, `dts_seconds`, `pts_seconds`, `duration_seconds`, `size`, `is_keyframe`, `offset`, `frame` |

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

`offset` locates the encoded sample within the file. `frame` is a typed, lazy
binary reference with source, track, sample, byte range, timestamp, and duration;
payload bytes are not embedded in JSON. Byte ranges are checked against media
data boxes. Missing sample-location tables produce null `offset` and `frame`
values. In the web API, frame values also include opaque IDs and preview/raw URLs.

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
- `app`: embedded HTML/CSS/JS workbench, uploads, JSON query API, exact raw
  sample downloads, and FFmpeg preview rendering.
- `cmd/bquery-web`: localhost server and graceful session cleanup.

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
listed, but fragment-derived duration and frames, compact
sample sizes (`stz2`), multiple sample
descriptions, edit-list timing, rotation, and compressed QuickTime movie headers
are not interpreted. Frame timing uses `stts`, `ctts` (including signed version
1 offsets), `stsz`, and `stss`. Byte locations use `stsc` plus `stco`/`co64`;
fragments do not contribute frame rows. External sample data references are not
supported: samples must reside in the source file's media data boxes.
Durations reflect declared headers, not decoded playback;
unknown declared durations remain null. Preview decoding is separate from native
query parsing; there is no semantic search of video contents. Format detection uses common leading box types,
followed by structural validation and a required movie box.

To add a format, implement `format.Adapter` and register it in the CLI. Future
work: fragmented video support, streaming scans and
pushdown, then additional format adapters.

Format references: [Apple QuickTime atoms](https://developer.apple.com/documentation/quicktime-file-format/atoms),
[track header](https://developer.apple.com/documentation/quicktime-file-format/track_header_atom),
[media header](https://developer.apple.com/documentation/quicktime-file-format/media_header_atom),
[composition offsets](https://developer.apple.com/documentation/quicktime-file-format/composition_offset_atom).
