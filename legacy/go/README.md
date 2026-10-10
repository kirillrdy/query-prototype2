# Archived Go implementation

This directory preserves the previous implementation and regression oracle.
The production project now lives in `../../src` and builds with Zig; see the
[current README](../../README.md). The instructions below describe the archived
version, including its obsolete SAM3 worker architecture.

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

Open the printed `http://127.0.0.1:8080` URL. Use **Open video** to upload files,
or enter a filesystem path under **Open local path** to read a video in place
without copying it. Registration checks only the file and its format header;
metadata is indexed when you press **Run query**. Frame counts appear after the
first frame query. Opening a file or refreshing the page does not run a query.
For example:

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

Use the **Table / Graph** toggle beside Results to plot numeric fields. Graph
view defaults to a timestamp on X and a motion score on Y when available; choose
other numeric fields with the axis selectors, or use row number on X. It updates
as rows arrive, skips null/non-numeric values, and breaks lines at missing values
and source/track boundaries when those identities are present. Hover a point for
its values, or click a point containing a frame to preview it. Switching views
keeps the same results and playback selection.

The **×** beside a source removes it from the session and from subsequent
queries. It never deletes the original video. All loaded sources participate
in queries; you can also use `WHERE path = 'video.mp4'` to select one source.

Previews require `ffmpeg` and `ffprobe` on PATH. Metadata queries and binary
downloads work without them. FFprobe maps decoded pictures to exact sample byte
positions, so B-frames are identified correctly; FFmpeg then decodes the source
with its required reference pictures and exports only the selected pictures.
The decoder seeks near the requested samples using the container's sync-sample
index, then maps exact byte positions in that local decode. Sparse inter-frame
selections decode nearby reference groups; keyframe-only batches skip inter-frame
decoding entirely. Indexing stops as soon as the requested samples are mapped.
Preview images are cached JPEGs, scaled to fit 1280 × 720 without enlarging.
The downloaded `.bin` is an encoded sample, which may require codec headers and
other frames to decode; it is not a standalone video file.

The server binds to localhost only; use `-port 8090` to choose another port.
The workbench streams complete rows as they become available, displays first-row
latency, and provides **Cancel query**. Received rows remain available if canceled
or interrupted, with an explicit incomplete-result notice. Results have no row
cap; use SQL `LIMIT` and `OFFSET` to select a page. The session accepts up to 16 files. Uploads stream
to disk with no fixed size limit; use a local path for large files to avoid
copying the entire video. Uploads and preview caches live in a temporary
directory and are deleted when the app exits normally. A removed source's
temporary copy/cache remains until session cleanup. Stop the app with Ctrl+C.
Files passed on the command line are read in place; changes detected after
loading require restarting or uploading again. Decoder work is serialized and
has a two-minute timeout per render request.

## Functions over frames

Both the CLI and workbench support expressions, `AS` aliases, `LAG` windows,
derived-table subqueries, and result ordering. Use the **Motion** example in the
query editor, or run:

```sql
SELECT frame, pts_seconds,
       motion(frame, LAG(frame) OVER (
         PARTITION BY path, track_id
         ORDER BY pts_seconds, sample_index
       )) AS motion_score
FROM frames
ORDER BY path, track_id, pts_seconds
LIMIT 100
```

`motion(a, b)` measures visual change as normalized mean absolute pixel
difference. Frames are decoded at exact sample positions into lossless 160 × 90
grayscale buffers; the score is `sum(abs(a - b)) / (160 * 90 * 255)`. Zero means
identical grayscale buffers; one means maximal difference. Resizing uses a
fixed analysis size. Changes in lighting and camera position also affect the
score, so it is a visual-change measure rather than a count of moving objects.
It returns null if either argument is null. Decoding requires FFmpeg/FFprobe;
`LAG` alone only moves references and needs no decoder.

`LAG(value [, offset [, default]]) OVER (...)` returns the value from an earlier
row in the same partition. Offset defaults to 1, must be a nonnegative integer
or null, and may be a column expression; 0 returns the current row. Out-of-range
rows return the default, or null if omitted. A null offset returns null. Null
values are respected, not skipped. Partition and order clauses are optional;
without ordering, rows follow their input order. Equal order keys retain input
order; include `sample_index` to make presentation-time ordering explicit.
Window ordering does not itself reorder the final results.

`WHERE` filters **before** window evaluation. Final ordering, `OFFSET`, and
`LIMIT` apply **after** window evaluation; skipped rows remain available to LAG. To keep the preceding frame from outside a time
range, compute the window in a subquery and filter outside it. This also lets
you use the name `previous_frame` as an argument:

```sql
SELECT frame, pts_seconds, motion(frame, previous_frame) AS motion_score
FROM (
  SELECT frame, pts_seconds,
         LAG(frame) OVER (
           PARTITION BY path, track_id
           ORDER BY pts_seconds, sample_index
         ) AS previous_frame
  FROM frames
) AS sequenced
WHERE pts_seconds >= 1 AND pts_seconds < 2
ORDER BY pts_seconds
LIMIT 100
```

Aliases cannot refer to another expression in the same SELECT list. Use the
direct nested call or a subquery as above. To filter on `motion_score`, put its
calculation in a subquery and apply `WHERE motion_score > 0.05` outside it.
Like other window functions, LAG cannot appear in WHERE or inside another LAG.
Result ordering accepts input column names and output aliases, with ASC/DESC and
NULLS FIRST/LAST (defaults: LAST for ASC, FIRST for DESC).

### Object detection

`person_count(frame)` counts detected people. `object_count(frame, 'car')` counts
a named object class supported by the loaded model. Class names are case
insensitive; unknown names produce an error. Null frames or class names return
null. Counts represent detections in individual frames, not unique people across
a video. The **People** workbench example checks the first 100 frames of each
video track (only about four seconds at 24 fps):

```sql
SELECT frame, path, pts_seconds, person_count(frame) AS people
FROM frames
WHERE sample_index <= 100
  AND person_count(frame) > 0
ORDER BY path, pts_seconds
```

For every frame in a later section, replace `sample_index <= 100` with a time
range such as `pts_seconds >= 60 AND pts_seconds < 70`.
For large videos, time ranges can reduce detection work. With `LIMIT`, detection
filters scan small batches and stop after enough matches when there are no
window functions and ordering uses input fields (such as `path, pts_seconds`).
`OFFSET` counts matching rows. Ordering by a computed value or using windows
requires the relevant complete input. Detection calls are automatically split
into batches of at most 32 frames; long scans are not limited to 4,096 frames.
Detection queries have no fixed duration limit; use **Cancel query** to stop a
long scan. Browser disconnects and CLI Ctrl+C also cancel detection.

Detection works with the normal CLI and web commands; no detection settings are
required. It runs locally on CPU through Go bindings to ONNX Runtime, with no
Python, pip, C compiler, or manual model installation:

```sh
go run ./cmd/bquery-web video.mp4
# Or use the same functions in the CLI:
go run ./cmd/bquery -query 'SELECT pts_seconds, person_count(frame) AS people FROM frames LIMIT 10' video.mp4
```

The first detection query automatically downloads the YOLO11n ONNX model and the
official Microsoft ONNX Runtime CPU library, verifies their pinned SHA-256
checksums, and caches them under the user cache directory's
`binaryquery/detection` folder (on Linux, typically `~/.cache/binaryquery/detection`).
First use needs internet access and downloads about 19 MB on Linux amd64. Later
queries reuse the cache and run offline. Interrupted downloads are discarded
and retried on the next query. No downloads happen for metadata or motion queries.
FFmpeg and FFprobe are still needed for frame decoding.

Automatic detection supports Linux and macOS on amd64 and arm64. The Go binary
builds with `CGO_ENABLED=0`; inference uses the automatically managed native
ONNX Runtime library. The default model recognizes the 80 COCO classes, including
people, cars, buses, bicycles, and animals. Detections use exact-sample preview
JPEGs, RGB letterboxing at 640 × 640, and class-aware nonmaximum suppression
with an IoU threshold of 0.7 and at most 300 detections per frame. Small or obscured
objects may be missed.

Optional overrides, set before starting the CLI or server:

- `BQUERY_DETECTION_CACHE`: directory for cached model and runtime assets.
- `BQUERY_DETECTION_CONFIDENCE`: threshold greater than zero and at most one;
  defaults to `0.25`.
- `BQUERY_DETECTION_MODEL`: existing local ONNX model with one RGB NCHW
  `[1,3,640,640]` input and a COCO detection output `[1,84,N]` or `[1,N,6]`.
  PyTorch `.pt` files are not supported; unset the override to use the default.

The model stays loaded across queries. Temporary analysis JPEGs are released
after each inference batch; preview images remain separate. Counts for all classes are cached together
for up to 4,096 frames, so filtering and projecting counts, or querying another
class, reuse inference. Source files are checked even on cache hits. Downloads,
decoding, and work between inference calls respect cancellation; an active native
inference call finishes before cancellation returns. Stopping the server releases
the detector's native resources.

For optional real-model API coverage, set `BQUERY_TEST_DETECTION_IMAGE` to a local
image known to contain people and run `go test ./app -run TestPeopleDetectionIntegration`.
The test uses the same automatic setup as ordinary queries.

### SAM3 text segmentation

`sam3(frame, 'person')` runs the native Zig SAM3 library in `../sam3` with a
free-form text prompt. `sam3_count(frame, 'person')` returns the number of matching
masks and `sam3_score(frame, 'person')` returns the highest accepted score (zero
when no masks match). All three functions return null for a null frame or prompt.
They work in SELECT and WHERE, in both the CLI and web app.

Build the worker with Zig 0.17 and select the GPU backend:

```sh
cd sam3-worker
zig build -Doptimize=ReleaseFast             # OpenCL on Linux, Metal on macOS
# For NVIDIA: zig build -Doptimize=ReleaseFast -Dbackend=cuda -Dsm=sm_89
cd ..
export BQUERY_SAM3_WORKER="$PWD/sam3-worker/zig-out/bin/bquery-sam3"
go run ./cmd/bquery-web video.mp4
```

The local package path in `sam3-worker/build.zig.zon` expects `query` and `sam3`
to be siblings; adjust it if your checkout lives elsewhere. Alternatively,
install `bquery-sam3` on PATH or alongside the bquery executable. SAM3 needs a
working GPU driver for the selected backend; its library downloads missing
model files into `~/.cache/sam3-zig` using curl on first inference. These models
require several gigabytes of disk and GPU memory. Ordinary queries do not start
the worker or download SAM3 models.

```sql
SELECT frame, pts_seconds, sam3_count(frame, 'person wearing a hat') AS people
FROM frames
WHERE is_keyframe = true AND sam3_count(frame, 'person wearing a hat') > 0
LIMIT 10
```

For masks, use `SELECT frame, sam3(frame, 'car') AS segmentation FROM frames
WHERE is_keyframe = true LIMIT 1`. The result contains `count`, `score`, `width`,
`height`, and `masks`. Each mask has a confidence `score` and `rle`: row-major
run lengths alternating background and foreground, starting with background
(which can have length zero). Runs cover exactly `width * height` pixels;
positive logits define foreground. Mask coordinates cover the whole decoded
image at the returned mask resolution; scale them to the frame dimensions.

Set `BQUERY_SAM3_CONFIDENCE` to change the acceptance threshold (default `0.5`,
greater than zero and at most one). Inference is serialized through a persistent
worker; the most recent image embedding and text features are reused, and a
bounded cache shares results across the three functions. Exact samples decode
to lossless PNGs capped at 1280x720 while preserving aspect ratio, in batches of
at most eight argument tuples. Temporary analysis images are removed after each
batch. SAM3 queries have no fixed duration limit; cancellation stops the worker,
which restarts lazily on the next query.

For optional real-model coverage, set `BQUERY_TEST_SAM3_IMAGE` to a cat image and
run `go test ./video -run TestSAM3NativeIntegration -v` with the worker configured.
Set `BQUERY_TEST_SAM3_VIDEO` to a video and run
`go test ./app -run TestSAM3FrameIntegration -v` to test exact-frame decoding and
the query API together.

Scalar functions use the engine's `Functions` registry and evaluate batches of
argument tuples. A function's optional `MaxBatchSize` bounds each invocation.
Motion deduplicates frame references and decodes each source
track in a batch; the grayscale cache is shared with the preview decoder's
sample index. With a simple SELECT and LIMIT, only frames needed for the result
are decoded. Filtering or ordering by a computed score may require evaluating
more rows. A motion batch is capped at 4,096 distinct frames; narrow the input
range or use LIMIT before expensive calculations for larger videos. Queries
without object detection or SAM3 have a two-minute timeout. Window layouts and subquery results
need their full relevant input before dependent rows can be produced. Filters
with input-field ordering stream matching rows after each small scan batch,
including when no LIMIT is set. Ordering
by a computed score also evaluates that score before ordered rows can stream.
Ordinary scalar projections stream a first row, then batches of 8 and 32 rows;
`LAG` retains its complete partition across these batches. The first row of a
motion/LAG partition can be emitted with a null score before any decoding.

The web UI uses `POST /api/query/stream` with a JSON `{"sql": "..."}` body.
Its `application/x-ndjson` response contains `schema` (columns), `rows` (complete
row batches), then `done` (completion), or `error` after a partial result.
Each event is flushed immediately. `POST /api/query` retains the original
complete JSON response for other clients.

## Run

Requires Go 1.26 or newer. Go module dependencies are fetched automatically by Go.

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
SELECT * | expression [AS alias] [, expression [AS alias] ...]
FROM table | (SELECT ...) AS alias
[WHERE predicate [AND ...]]
[ORDER BY column_or_alias [ASC | DESC] [NULLS FIRST | LAST] [, ...]]
[LIMIT nonnegative_integer]
[OFFSET nonnegative_integer]

predicate := expression operator expression
           | expression [NOT] BETWEEN lower_expression AND upper_expression
           | expression IS [NOT] NULL
```

Keywords and identifiers are case insensitive. Operators: `=`, `!=`, `<>`, `<`,
`<=`, `>`, `>=`; `IS NULL`, `IS NOT NULL`, `BETWEEN`, and `NOT BETWEEN` are also supported.
`BETWEEN` includes both endpoints; its internal AND is part of the range, so
additional predicates may follow another AND. Bounds can be literals, columns,
or scalar function expressions. Null comparisons retain SQL unknown semantics.
`OFFSET n` skips n rows after filtering and ordering, before LIMIT and projection.
It works without LIMIT, or with LIMIT and OFFSET in either order; both require
nonnegative integers. Use ORDER BY for stable pagination. For example:

```sql
SELECT frame, pts_seconds
FROM frames
WHERE pts_seconds BETWEEN 10 AND 20
ORDER BY pts_seconds, sample_index
LIMIT 50 OFFSET 100
```

Literals are
numbers, `null`, `true`/`false`, or single-quoted strings (escape a quote
as `''`). Comparisons are type-sensitive; unknown predicates do not match. A trailing
semicolon is accepted. Expressions support columns, literals, registered scalar
functions (`motion`, `person_count`, `object_count`), and `LAG(...) OVER (...)`. No joins, aggregation,
OR, arithmetic operators, CTEs, qualified/quoted identifiers, named windows,
window-frame clauses, or other window functions yet. Subqueries require an
alias. Row order follows input order unless a final ORDER BY is supplied.

## Architecture and boundaries

- `format`: source, schema, row, and adapter interfaces.
- `format/mp4`: bounded box traversal, headers, sample descriptions, and sample timing.
- `engine`: expressions, schema validation, filtering, LAG windows, batch
  function evaluation, subqueries, ordering, projection, and limit.
- `video`: reusable exact-sample decoder, preview/grayscale representations,
  motion, and an automatically initialized local YOLO ONNX detector.
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
Registration does not parse the movie or create frame rows. Queries over
`files`, `tracks`, and `boxes` skip sample-table contents. Frame queries build
and cache compact sample metadata at query time, then produce column values
on demand without creating a map for every frame. Simple scans with scalar
comparisons stop after LIMIT matches. Sorting, windows, and computed filters
need the complete relevant input; subquery results are materialized. LIMIT does
not stop the initial sample index validation. Media bytes are read only for
binary downloads or decoding. The one-million-sample bound still applies to
frame indexing, independently of the file size.

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
query parsing; object detection uses an optional model rather than metadata. Format detection uses common leading box types,
followed by structural validation and a required movie box.

To add a format, implement `format.Adapter` and register it in the CLI. Future
work: fragmented video support, deeper sample-table pushdown, more frame
functions, then additional format adapters.

Window semantics follow the SQL behavior documented in
[PostgreSQL LAG](https://www.postgresql.org/docs/current/functions-window.html)
and its [window evaluation tutorial](https://www.postgresql.org/docs/current/tutorial-window.html).

Format references: [Apple QuickTime atoms](https://developer.apple.com/documentation/quicktime-file-format/atoms),
[track header](https://developer.apple.com/documentation/quicktime-file-format/track_header_atom),
[media header](https://developer.apple.com/documentation/quicktime-file-format/media_header_atom),
[composition offsets](https://developer.apple.com/documentation/quicktime-file-format/composition_offset_atom).
