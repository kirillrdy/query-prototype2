# Binary Query

A native Zig query engine and local web workbench for MP4/MOV files. Query
container metadata, exact encoded frame references, motion, object counts,
and SAM3 text segmentation. SAM3 is imported directly from `../sam3` and runs
inside the CLI/server process; no SAM3 worker or Python service is needed.

## Build and run

Use Zig 0.17 with this checkout and `~/sam3` as sibling directories. The local
SAM3 dependency is declared in `build.zig.zon`; adjust its path if necessary.

```sh
zig build -Doptimize=ReleaseFast
./zig-out/bin/bquery-web video.mp4 other.mov
./zig-out/bin/bquery -query 'SELECT path, sample_index, pts_seconds FROM frames LIMIT 10' video.mp4

# Build and run directly:
zig build run -- -query 'SELECT * FROM tracks' video.mp4
zig build web -- video.mp4

# NVIDIA backend instead of the default OpenCL (Linux) or Metal (macOS):
zig build -Doptimize=ReleaseFast -Dbackend=cuda -Dsm=sm_89
```

Both executables are native Zig. Metadata and encoded-byte downloads work
without FFmpeg or an initialized GPU. Previews, motion, and inference over
frames require `ffmpeg` and `ffprobe` on PATH. SAM3 requires a working GPU for
the selected backend. Its model assets are loaded lazily, with missing files
downloaded using curl into `~/.cache/sam3-zig` on first inference.

CLI flags precede file paths. Results contain an ordered `columns` array and
JSON objects in `rows`. All supplied files participate in one query; LIMIT
applies globally. The default query is `SELECT * FROM frames LIMIT 100`.
Any input error fails the command before results are emitted.

## Web workbench

Open the printed `http://127.0.0.1:3000` URL; `-port 8090` chooses another port.
Use **Open video** to upload a file or **Open local path** to read it in place.
Registration checks only the file and format header. Metadata is indexed when
you run a query; frame counts appear after the first frame query.

```sql
SELECT frame, path, sample_index, pts_seconds
FROM frames
WHERE pts_seconds >= 0.5 AND pts_seconds < 2
LIMIT 100
```

Results have **Table**, **Graph**, and **Player** tabs. Click a frame cell or a
graph point to open its preview in **Player** and download its exact encoded
sample bytes. Select rows in **Table**, then use **Play** in **Player**
for an image sequence, without audio. Switching tabs preserves the selection
and pauses playback when leaving the player.
Player keeps the frame, masks, and playback bar visible. Expand **Details** for
frame metadata, speed, playback order, and the binary download.
Playback can follow presentation order or result order, with scrubbing, speed
controls, and single-frame stepping. **Graph** plots numeric fields as results
arrive.
SAM3 masks overlay the frame preview automatically when the same row contains
one frame and a segmentation result. Use **Show masks** and **Opacity** to adjust
the overlay. Different masks use different colors; with multiple segmentation
columns, choose which one to display. Overlays also follow selected-frame
playback. Object results remain available as JSON in the table.

The **×** beside a source removes it from the session without deleting the
original video. All loaded sources participate in queries. Sources changed
on disk must be reopened. The server binds to localhost, accepts at most 16
sources, and checks same-origin requests. Uploads stream to disk without a
fixed size limit. Temporary uploads and decoded images are removed on exit.

Queries stream complete rows, show first-row latency, and offer **Cancel query**.
Received rows remain available after interruption with an incomplete-result
notice. There is no result row cap; use LIMIT/OFFSET for pagination. Browser
disconnects and CLI Ctrl+C cancel work. Metadata, motion, and rendering have a
two-minute deadline; detection and SAM3 queries have no fixed duration limit.
An active native inference call finishes before cancellation returns.

Previews are cached JPEGs fitted within 1280 × 720 without enlargement. FFprobe
maps decoded pictures to sample byte positions, including B-frames; FFmpeg seeks
near the sample's preceding sync frame and exports the exact selected picture.
Downloaded `.bin` samples may need codec headers and reference frames to decode.
They are not standalone videos.

## Functions over frames

### SAM3 text segmentation

```sql
SELECT frame, pts_seconds,
       sam3_count(frame, 'person wearing a hat') AS people,
       sam3_score(frame, 'person wearing a hat') AS confidence
FROM frames
WHERE is_keyframe = true
  AND sam3_count(frame, 'person wearing a hat') > 0
LIMIT 10
```

`sam3(frame, 'car')` returns `count`, `score`, `width`, `height`, and `masks`.
`sam3_count` returns the number of accepted masks; `sam3_score` returns their
highest score, or zero when none match. Null frames or prompts return null.
These functions work in SELECT and WHERE, in the CLI and web workbench.

Each mask contains a confidence `score` and `rle`: row-major run lengths
alternating background and foreground, starting with background (which can
have length zero). Runs cover exactly `width * height` pixels; positive logits
are foreground. Scale mask coordinates to the decoded frame dimensions.
Exact samples decode to lossless PNGs fitted within 1280 × 720.

To view the masks, return the frame and segmentation together and click the
frame cell. The segmentation alias is arbitrary; no additional SQL is needed:

```sql
SELECT frame, sam3(frame, 'hat') AS segmentation
FROM frames
WHERE sample_index = 3000
LIMIT 1
```

`src/video.zig` uses `@import("sam3")` directly. The model stays loaded across
queries. The latest image embedding, up to 32 text features, and up to 32
segmentation results are reused; count, score, and masks share one inference
result. Set `BQUERY_SAM3_CONFIDENCE` to a value greater than zero and at most one
to change the default acceptance threshold of `0.5`.

### Motion and windows

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

`motion(a, b)` is normalized mean absolute difference between exact samples
decoded as lossless 160 × 90 grayscale buffers:
`sum(abs(a - b)) / (160 * 90 * 255)`. Zero means identical buffers; one means
maximal difference. Lighting and camera movement also affect the score. Null
arguments return null. LAG alone moves frame references without decoding.

`LAG(value [, offset [, default]]) OVER (...)` returns an earlier value from the
same partition. Offset defaults to one, may be an expression, and must be a
nonnegative integer or null. Zero returns the current value; a null offset
returns null. Out-of-range rows return the default or null. Null values are
respected. Partition and ordering clauses are optional; equal ordering keys
retain input order. Window ordering does not reorder final results.

WHERE filters **before** windows. Final ORDER BY, OFFSET, and LIMIT apply
**after** windows. To retain a preceding frame outside a time range, use a
subquery:

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

Aliases cannot refer to another expression in the same SELECT list. Use a
subquery to filter on computed aliases. LAG cannot appear in WHERE or inside
another LAG. Ordering accepts input columns and output aliases, ASC/DESC, and
NULLS FIRST/LAST; defaults are LAST for ASC and FIRST for DESC.

### Object detection

`person_count(frame)` counts people; `object_count(frame, 'car')` counts a
case-insensitive COCO class. Unknown classes produce errors; null arguments
return null. Counts describe each picture, not unique objects across a video.

```sql
SELECT frame, path, pts_seconds, person_count(frame) AS people
FROM frames
WHERE pts_seconds >= 60 AND pts_seconds < 70
  AND person_count(frame) > 0
ORDER BY path, pts_seconds
LIMIT 10
```

YOLO11n runs locally on CPU through direct Zig bindings to ONNX Runtime.
The first detection query downloads the model and official runtime library,
verifies pinned SHA-256 checksums, and caches them in
`~/.cache/binaryquery/detection` (or the XDG cache directory). Downloads use curl
and runtime extraction uses tar. Later queries run offline. Automatic assets
support Linux and macOS on amd64 and arm64. The model recognizes 80 COCO classes.

Optional environment overrides:

- `BQUERY_DETECTION_CACHE`: directory for model/runtime assets.
- `BQUERY_DETECTION_CONFIDENCE`: threshold in `(0, 1]`, default `0.25`.
- `BQUERY_DETECTION_MODEL`: local ONNX model with one RGB NCHW `[1,3,640,640]`
  input and output `[1,84,N]` or `[1,N,6]`.

Detection uses exact-sample JPEGs, RGB letterboxing, class-aware nonmaximum
suppression with IoU `0.7`, and at most 300 detections. All class counts are
cached together for up to 4,096 frames. Source changes are checked on cache hits.

Cheap input predicates run before inference. With input-field ordering and no
windows, simple scans stop after enough LIMIT matches. Computed ordering,
windows, and subqueries require their complete relevant input. Decoder and
inference work is serialized; models run inside the query process.

## Tables

| Table | Columns |
| --- | --- |
| `files` | `path`, `format`, `size`, `major_brand`, `duration_seconds`, `track_count` |
| `tracks` | `path`, `track_id`, `handler`, `codec`, `width`, `height`, `timescale`, `duration_seconds` |
| `boxes` | `path`, `box_path`, `type`, `offset`, `size`, `depth` |
| `frames` | `path`, `track_id`, `sample_index`, `dts_seconds`, `pts_seconds`, `duration_seconds`, `size`, `is_keyframe`, `offset`, `frame` |

Offsets/sizes are bytes, dimensions are pixels, and durations are seconds.
Handlers are four-byte codes (`vide`, `soun`); codecs are the first sample-entry
code (`avc1`, `hvc1`, `mp4a`, etc.). Missing metadata is null. Box paths describe
ancestry and can repeat; offsets identify individual boxes. MP4 and MOV both
report format `mp4`.

Frames contain video samples in decode order with indices starting at one per
track. DTS begins at zero; PTS includes composition offsets. Edit lists are not
applied. When the sync table is absent, all samples are sync samples. `frame`
is a typed lazy reference containing source, track, sample, byte range,
timestamp, and duration. Payload bytes are not embedded in JSON. Missing sample
locations yield null offset/frame. Web references also carry IDs and URLs.

## Query language and API

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

Keywords and identifiers are case insensitive. Operators are `=`, `!=`, `<>`,
`<`, `<=`, `>`, `>=`. BETWEEN includes both endpoints. Comparisons are type
sensitive, preserve exact 64-bit integer ordering, and use SQL null semantics.
Literals are numbers, null, booleans, and single-quoted strings with quotes
escaped as `''`. A trailing semicolon is accepted. LIMIT and OFFSET can appear
in either order. Subqueries require aliases. There are no joins, aggregation,
OR, arithmetic, CTEs, qualified identifiers, or other window functions.

`POST /api/query` accepts `{"sql":"..."}` and returns complete JSON.
`POST /api/query/stream` accepts the same body and returns flushed NDJSON events:
`schema` with columns, `rows` with complete rows, then `done`, or `error` after
partial results. The existing workbench uses the streaming endpoint.

## Implementation and validation

- `src/types.zig`: lazy tables, scalar values, typed frame references, control.
- `src/mp4.zig`: bounded box traversal, headers, sample timing/byte locations.
- `src/parse.zig`, `src/engine.zig`: SQL, validation, windows, filtering, ordering.
- `src/video.zig`: exact-sample decoding, motion, direct SAM3, object detection.
- `src/ort.zig`, `src/detection_assets.zig`: native CPU runtime and verified assets.
- `src/web.zig`, `src/static/`: localhost API and embedded workbench.
- `src/main.zig`, `src/web_main.zig`: executable entry points.

Metadata queries skip media payloads. Frame queries validate and cache compact
sample indices, then produce column values lazily. Sorting, windows, and
subqueries may materialize rows. Parser limits are 32 nesting levels, 100,000
boxes, and 1,000,000 samples per file. Headers support 32/64-bit and parent-sized
boxes, UUIDs, timing versions 0/1, signed composition offsets, stsc, stco/co64,
and media-range validation. Fragment-derived frames, stz2, multiple sample
descriptions, edit lists, rotation, and compressed QuickTime headers remain
outside the adapter's scope.

```sh
zig build test

# Browser mask decoding/scaling tests (Node.js needed only for these tests):
node --test src/static/masks.test.cjs
# Optional preview interaction test (also needs Firefox):
node src/static/masks.browser.test.cjs

# Optional real-model checks (GPU required for SAM3):
BQUERY_TEST_SAM3_VIDEO=/path/to/video.mp4 zig build test
BQUERY_TEST_DETECTION_VIDEO=/path/to/video.mp4 zig build test

# Differential regression tests against the archived Go implementation:
zig build parity
cd legacy/go
go test ./...
```

`legacy/go` preserves the prior implementation and its tests as a regression
oracle, including its old worker code. It is independent of the production
Zig build. Differential tests cover SQL semantics, valid/malformed MP4 metadata,
web uploads and streams, exact B-frame previews/raw bytes, and motion scores.
