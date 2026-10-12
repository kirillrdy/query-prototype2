# Binary Query

A native Zig query engine and local web workbench for MP4/MOV files. Query
container metadata, exact encoded frame references, motion, object counts,
and SAM3 text segmentation. SAM3 is imported as a Zig dependency and runs
inside the CLI/server process; no SAM3 worker or Python service is needed.

## Build and run

Use Zig 0.17. The SAM3 dependency is pinned to a GitHub commit in
`build.zig.zon` and downloaded automatically by Zig on the first build.
The shared native ONNX runtime is pinned separately in `build.zig.zon`, including
the interior-axis Softmax support required by YOLO11n.

```sh
git clone https://github.com/kirillrdy/query.git
cd query
zig build -Doptimize=ReleaseSmall
./zig-out/bin/bquery-web
./zig-out/bin/bquery -query "SELECT path, frame_index, pts_seconds FROM frames('video.mp4') LIMIT 10"

# Build and run directly:
zig build run -- -query "SELECT * FROM tracks('video.mp4')"
zig build web

# NVIDIA backend instead of the default OpenCL (Linux) or Metal (macOS):
zig build -Doptimize=ReleaseFast -Dbackend=cuda -Dsm=sm_89
```

Both executables are native Zig. Metadata and encoded-byte downloads work
without FFmpeg or an initialized GPU. Previews, motion, and inference over
frames require `ffmpeg` and `ffprobe` on PATH. SAM3 and object detection require a working GPU for
the selected backend. Its model assets are loaded lazily, with missing files
downloaded using curl into `~/.cache/sam3-zig` on first inference.

The CLI prints aligned tables, with 24 rows per page in a terminal. Press Enter
for the next page or `q` then Enter to stop paging. Use `--page-size N` to change
the page size or `--no-pager` to print everything. Redirected output never pauses.
Use `--json` for complete results with an ordered `columns` array and JSON
objects in `rows`, including full frame metadata and mask RLEs. Table cells are
limited to 64 bytes plus an ellipsis; frame cells show their presentation index.
Table functions open their named local file directly, in the CLI and web app.
Plain table names also accept positional CLI input files, with LIMIT applying
globally.
Use either a filename in SQL or positional CLI files in a query.
Any input error fails the command before results are emitted.

Run `./bquery` without a query to enter the SQL console. Statements can span
multiple lines and execute at `;`. The console keeps models and inference caches
loaded between queries; query errors return to the prompt.

```sql
bquery=> SELECT frame, path, frame_index, pts_seconds
bquery-> FROM frames('/home/kirillvr/holes_3min.mp4')
bquery-> LIMIT 48;
```

Use `\q` to quit, `\h` for help, `\g` to execute the current query without a
semicolon, `\p` to print it, or `\r` to clear it. `\pset pager on|off` and
`\pset format table|json` change output options. `./bquery video.mp4` starts the
console with a default input for plain table names such as `FROM frames`.
SQL piped to stdin also works without prompts; errors produce a nonzero exit.

## Web workbench

Open the printed `http://127.0.0.1:3000` URL; `-port 8090` chooses another port.
Enter the filename directly in SQL using `frames('video.mp4')`. Absolute paths
and paths relative to the server's working directory are supported. Escape
single quotes in filenames as `''`. The file is opened and indexed when the
query runs. The same syntax works for `files`, `tracks`, and `boxes`.

```sql
SELECT frame, path, frame_index, pts_seconds
FROM frames('video.mp4')
WHERE pts_seconds >= 0.5 AND pts_seconds < 2
LIMIT 100
```

Results have **Table**, **Graph**, and **Player** tabs. Click a frame cell or a
graph point to open its preview in **Player** and download its exact encoded
sample bytes. **Player** includes all frames returned by the query; use **Play**
for an image sequence, without audio. Switching tabs preserves the query frames
and pauses playback when leaving the player.
Player keeps the frame, masks, and playback bar visible. Expand **Details** for
frame metadata, speed, playback order, and the binary download.
Playback starts with the first frame and buffers two frames ahead, decoding
previews on demand instead of preparing every query frame.
Playback can follow presentation order or result order, with scrubbing, speed
controls, and single-frame stepping. **Graph** appends numeric points as results
arrive, keeping existing points and keyboard focus. Updates are batched to screen
refreshes; the hidden table catches up when you select **Table**.
SAM3 masks overlay the frame preview automatically when the same row contains
one frame and a segmentation result. Use **Show masks** and **Opacity** to adjust
the overlay. Different masks use different colors; with multiple segmentation
columns, choose which one to display. Overlays also follow frame
playback. Object results remain available as JSON in the table.

Each table function reads only its named file. Repeated queries reuse cached
metadata and frame references. The server binds to localhost, caches up to 16
sources, and checks same-origin requests. If a source changes on disk, restart
the server before querying it again. Decoded images are removed on exit.

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
FROM frames('video.mp4')
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
FROM frames('video.mp4')
WHERE frame_index = 3000
LIMIT 1
```

`src/video.zig` uses `@import("sam3")` directly and opens only the text-prompt
graphs, skipping the point-prompt encoder and decoder. The model stays loaded across
queries. The latest image embedding, up to 32 text features, and up to 32
segmentation results are reused; count, score, and masks share one inference
result. Set `BQUERY_SAM3_CONFIDENCE` to a value greater than zero and at most one
to change the default acceptance threshold of `0.5`.
Set `BQUERY_PROFILE_SAM3=1` when starting the CLI or server to log per-frame
model loading, frame decoding, image encoding, text encoding, mask decoding,
and RLE timings. The first inference includes model and GPU initialization.

### Persistent person-count indexes

List saved indexes in the console or web editor:

```sql
SHOW INDEXES;
```

This shows the index name, video path, expression, indexed frame count, and
status (`valid`, `stale`, or `unknown`). Older indexes without saved source paths
have `unknown` status; rebuilding them records the path for validity checks.
Unreadable or corrupt index files are also listed with their status. Listing
indexes requires no input video, decoding, or inference.

Build a reusable index in the SQL console, with `-c`, or in the web query editor:

```sql
CREATE INDEX people_idx
ON frames('/home/kirillvr/holes_3min.mp4') (person_count(frame))
WHERE frame_index <= 100;
```

Omit `WHERE` to index the entire video. Index filters accept frame metadata
predicates; inference expressions, ORDER BY, and pagination are not supported
in index definitions. Building the index runs detection once per covered frame
and shows completed frames, percentage, remaining frames, and a saving stage in
the CLI and web editor. CLI progress goes to stderr so JSON output stays valid.
The web stream reports preparation and waiting for inference immediately.
Each progress event flushes both the response body and socket buffers.
Unfiltered builds get their frame total directly from metadata and fetch each
frame reference as it is processed, without first materializing every frame.
Completion is reported after the index is saved. It returns the index name and
indexed frame count. Running the same definition again
atomically replaces the named index for that video.

Normal queries automatically use saved counts in both projections and filters:

```sql
SELECT frame, path, pts_seconds, person_count(frame) AS people
FROM frames('/home/kirillvr/holes_3min.mp4')
WHERE frame_index <= 100 AND person_count(frame) > 0
ORDER BY path, pts_seconds;
```

Counts persist across CLI, REPL, and server restarts. Partial indexes can cover
different frame ranges; uncovered frames use normal inference. Existing sessions
notice newly published indexes. Indexed counts need no decoder or detector.
Changes to the source's size or modification time, the configured model's path,
size or modification time, or the confidence threshold invalidate saved counts;
rebuild the index to cover the new configuration. The server still requires a
restart when a loaded source changes on disk.

Indexes are stored under `$XDG_CACHE_HOME/binaryquery/indexes` (default
`~/.cache/binaryquery/indexes`); `BQUERY_INDEX_CACHE` overrides this directory.
Only `person_count(frame)` is supported currently. The index stores computed
values to avoid repeated inference; metadata scans and sorting still run.

### Motion and windows

```sql
SELECT frame, pts_seconds,
       motion(frame, LAG(frame) OVER (
         PARTITION BY path, track_id
         ORDER BY pts_seconds, frame_index
       )) AS motion_score
FROM frames('video.mp4')
ORDER BY path, track_id, pts_seconds
LIMIT 100
```

`motion(a, b)` is normalized mean absolute difference between exact samples
decoded as lossless 160 × 90 grayscale buffers:
`sum(abs(a - b)) / (160 * 90 * 255)`. Zero means identical buffers; one means
maximal difference. Lighting and camera movement also affect the score. Null
arguments return null. LAG alone moves frame references without decoding.
Paginated window queries prepare motion inputs in batches of up to 32 output
rows. Each decode range reuses its encoded frame-position index, and one FFmpeg
process produces the requested lossless grayscale frames. The preceding LAG
frame is included even when it lies before OFFSET. Cached buffers are reused
by later queries.

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
           ORDER BY pts_seconds, frame_index
         ) AS previous_frame
  FROM frames('video.mp4')
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
FROM frames('video.mp4')
WHERE pts_seconds >= 60 AND pts_seconds < 70
  AND person_count(frame) > 0
ORDER BY path, pts_seconds
LIMIT 10
```

YOLO11n runs locally through the native Zig ONNX runtime also used by SAM3,
with the OpenCL, Metal, or CUDA backend selected at build time. Detection requires
a working GPU for that backend. The first detection query downloads the model,
verifies its pinned SHA-256 checksum, and caches it in
`~/.cache/binaryquery/detection` (or the XDG cache directory). Downloads use curl;
later queries run offline. The model recognizes 80 COCO classes. Custom models
must use operators supported by the native runtime. Existing person-count
indexes from the C runtime must be rebuilt because the inference backend and
precision can affect counts near the confidence threshold.

Optional environment overrides:

- `BQUERY_DETECTION_CACHE`: directory for model assets.
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
| `frames` | `path`, `track_id`, `frame_index`, `dts_seconds`, `pts_seconds`, `duration_seconds`, `size`, `is_keyframe`, `offset`, `frame` |

Offsets/sizes are bytes, dimensions are pixels, and durations are seconds.
Handlers are four-byte codes (`vide`, `soun`); codecs are the first sample-entry
code (`avc1`, `hvc1`, `mp4a`, etc.). Missing metadata is null. Box paths describe
ancestry and can repeat; offsets identify individual boxes. MP4 and MOV both
report format `mp4`.

Frames appear in presentation order with `frame_index` starting at one per
track. Indices follow increasing PTS; equal timestamps retain decode order.
PTS includes composition offsets. DTS and encoded byte offsets still describe
the original samples and can move backward between presentation indices.
The graph connects points in X order, preserving each point's frame reference.
Edit lists are not applied. When the sync table is absent, all samples are sync samples. `frame`
is a typed lazy reference containing source, track, sample, byte range,
timestamp, and duration. Payload bytes are not embedded in JSON. Missing sample
locations yield null offset/frame. Web references also carry IDs and URLs.

## Query language and API

```text
SELECT * | expression [AS alias] [, expression [AS alias] ...]
FROM table | table('filename') | (SELECT ...) AS alias
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
- `src/detection.zig`: object detection through the native Zig ONNX runtime.
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
node --test src/static/graph.test.cjs
# Optional preview interaction test (also needs Firefox):
node src/static/masks.browser.test.cjs

# GPU numerical regression tests run with zig build test.
# Optional real-model checks:
BQUERY_TEST_SAM3_VIDEO=/path/to/video.mp4 zig build test
BQUERY_TEST_DETECTION_VIDEO=/path/to/video.mp4 zig build test
# Optional batched-motion equivalence check (needs FFmpeg/FFprobe):
BQUERY_TEST_MOTION_VIDEO=/path/to/video.mp4 zig build test
```
