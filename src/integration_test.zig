const std = @import("std");
const t = @import("types.zig");
const mp4 = @import("mp4.zig");
const engine = @import("engine.zig");
const video = @import("video.zig");
const sam3 = @import("sam3");

test "forward decoder matches independent frames across gaps and restarts" {
    const path = t.env("BQUERY_TEST_DETECTION_VIDEO") orelse return error.SkipZigTest;
    var arena = std.heap.ArenaAllocator.init(std.testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const c = t.Control{ .io = std.testing.io };
    const tables = try mp4.read(a, c, path, path, "frames");
    const table = tables.get("frames").?;
    if (table.count < 4) return error.SkipZigTest;
    var runtime = try video.Runtime.init(std.testing.allocator, c.io);
    defer runtime.deinit();
    const rows = [_]usize{ 0, 1, 2, @min(19, table.count - 1), @min(173, table.count - 1), @min(197, table.count - 1), @min(240, table.count - 1) };
    for (rows) |row| {
        const frame = table.value(row, "frame").frame;
        const bytes = try runtime.detectionJpeg(c, frame);
        var actual = try sam3.decodeImage(a, bytes);
        defer actual.deinit(a);
        const independent = try runtime.decode(a, c, frame, .detection);
        const expected_bytes = try std.Io.Dir.cwd().readFileAlloc(c.io, independent, a, .limited(32 << 20));
        var expected = try sam3.decodeImage(a, expected_bytes);
        defer expected.deinit(a);
        const lhs = sam3.RgbImage.fromImage(actual);
        const rhs = sam3.RgbImage.fromImage(expected);
        try std.testing.expectEqual(rhs.width, lhs.width);
        try std.testing.expectEqual(rhs.height, lhs.height);
        try std.testing.expectEqualSlices(u8, rhs.pixels, lhs.pixels);
    }
    try std.testing.expectEqual(@as(usize, 1), runtime.decoder_starts);
    _ = try runtime.detectionJpeg(c, table.value(0, "frame").frame);
    try std.testing.expectEqual(@as(usize, 2), runtime.decoder_starts);
    var other_source = table.value(0, "frame").frame;
    other_source.source = try std.fmt.allocPrint(a, "{s}/./{s}", .{ std.fs.path.dirname(path) orelse ".", std.fs.path.basename(path) });
    _ = try runtime.detectionJpeg(c, other_source);
    try std.testing.expectEqual(@as(usize, 3), runtime.decoder_starts);
    _ = try runtime.detectionJpeg(c, table.value(0, "frame").frame);
    try std.testing.expectEqual(@as(usize, 4), runtime.decoder_starts);
    const Cancel = struct {
        fn check(_: *anyopaque) bool {
            return true;
        }
    };
    var canceled: u8 = 0;
    const stop = t.Control{ .io = c.io, .check_context = &canceled, .check_fn = Cancel.check };
    try std.testing.expectError(error.Canceled, runtime.detectionJpeg(stop, table.value(1, "frame").frame));
    try std.testing.expect(runtime.decoder == null);
}
test "batched motion matches independent exact-sample decoding across a paginated window" {
    const path = t.env("BQUERY_TEST_MOTION_VIDEO") orelse return error.SkipZigTest;
    var arena = std.heap.ArenaAllocator.init(std.testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const c = t.Control{ .io = std.testing.io };
    const tables = try mp4.read(a, c, path, path, "frames");
    const count = tables.get("frames").?.count;
    if (count < 4) return error.SkipZigTest;
    const sql = try std.fmt.allocPrint(a, "SELECT frame_index, motion(frame, LAG(frame) OVER (PARTITION BY path, track_id ORDER BY pts_seconds, frame_index)) AS score FROM frames ORDER BY path, track_id, pts_seconds LIMIT 8 OFFSET {d}", .{@max(1, count / 2)});
    const query = try engine.parse(a, sql);
    var baseline = try video.Runtime.init(std.testing.allocator, c.io);
    defer baseline.deinit();
    var functions = try baseline.functions(a);
    functions.getPtr("motion").?.prepare = null;
    const expected = try engine.execute(a, c, query, tables, functions, null);
    var batched = try video.Runtime.init(std.testing.allocator, c.io);
    defer batched.deinit();
    const actual = try engine.execute(a, c, query, tables, try batched.functions(a), null);
    try std.testing.expect(actual.rows.len > 0);
    try std.testing.expectEqual(expected.rows.len, actual.rows.len);
    for (expected.rows, actual.rows) |old, new| {
        try std.testing.expectEqual(old[0].int, new[0].int);
        try std.testing.expectEqual(old[1], new[1]);
    }
    // Repeat with cached grayscale buffers and the same frame-position index.
    const cached = try engine.execute(a, c, query, tables, try batched.functions(a), null);
    for (actual.rows, cached.rows) |cold, warm| try std.testing.expectEqual(cold[1], warm[1]);
}
test "SAM3 directly segments an exact video sample" {
    const path = t.env("BQUERY_TEST_SAM3_VIDEO") orelse return error.SkipZigTest;
    var arena = std.heap.ArenaAllocator.init(std.testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const io = std.testing.io;
    const c = t.Control{ .io = io };
    const tables = try mp4.read(a, c, path, path, "frames");
    var runtime = try video.Runtime.init(std.testing.allocator, io);
    defer runtime.deinit();
    const query = try engine.parse(a, "SELECT sam3(frame, 'person') AS masks, sam3_count(frame, 'person') AS count, sam3_score(frame, 'person') AS score FROM frames WHERE is_keyframe = true LIMIT 1");
    const result = try engine.execute(a, c, query, tables, try runtime.functions(a), null);
    try std.testing.expectEqual(@as(usize, 1), result.rows.len);
    // Exercise the same result serialization used by the CLI and web API.
    const encoded = try std.json.Stringify.valueAlloc(a, result, .{});
    const decoded = try std.json.parseFromSlice(std.json.Value, a, encoded, .{});
    try std.testing.expectEqual(std.json.Value.object, std.meta.activeTag(decoded.value.object.get("rows").?.array.items[0].object.get("masks").?));
    const mask_result = try std.json.parseFromSlice(video.Segmentation, a, result.rows[0][0].json, .{});
    const masks = mask_result.value;
    try std.testing.expectEqual(@as(i128, @intCast(masks.count)), result.rows[0][1].int);
    try std.testing.expectEqual(@as(f64, masks.score), result.rows[0][2].float);
    try std.testing.expect(masks.width > 0 and masks.height > 0);
    for (masks.masks) |mask| {
        var pixels: u64 = 0;
        for (mask.rle) |run| pixels += run;
        try std.testing.expectEqual(@as(u64, @intCast(masks.width * masks.height)), pixels);
    }
}
test "native Zig ONNX object detection runs without a SAM3 worker" {
    const path = t.env("BQUERY_TEST_DETECTION_VIDEO") orelse return error.SkipZigTest;
    var arena = std.heap.ArenaAllocator.init(std.testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const io = std.testing.io;
    const c = t.Control{ .io = io };
    const tables = try mp4.read(a, c, path, path, "frames");
    var runtime = try video.Runtime.init(std.testing.allocator, io);
    defer runtime.deinit();
    const query = try engine.parse(a, "SELECT yolo(frame, 'person') AS people, yolo(frame, 'car') AS cars, person_count(frame) AS legacy_people, object_count(frame, 'car') AS legacy_cars FROM frames WHERE is_keyframe = true LIMIT 1");
    const result = try engine.execute(a, c, query, tables, try runtime.functions(a), null);
    try std.testing.expectEqual(@as(usize, 1), result.rows.len);
    try std.testing.expect(result.rows[0][0].int >= 0 and result.rows[0][1].int >= 0);
    try std.testing.expectEqual(result.rows[0][0], result.rows[0][2]);
    try std.testing.expectEqual(result.rows[0][1], result.rows[0][3]);
    try std.testing.expectEqual(@as(usize, 1), runtime.decoder_starts);
    try std.testing.expect(runtime.decoder == null);
}

test "person-count index serves fresh sessions and falls back outside its frame range" {
    const path = t.env("BQUERY_TEST_DETECTION_VIDEO") orelse return error.SkipZigTest;
    var arena = std.heap.ArenaAllocator.init(std.testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const io = std.testing.io;
    const c = t.Control{ .io = io };
    const tables = try mp4.read(a, c, path, path, "frames");
    var writer = try video.Runtime.init(std.testing.allocator, io);
    defer writer.deinit();
    writer.person_indexes.directory = try std.fmt.allocPrint(a, "{s}/indexes", .{writer.temp});
    const query = try engine.parse(a, "SELECT frame_index, yolo(frame, 'person') AS people FROM frames WHERE frame_index <= 3 ORDER BY frame_index");
    const baseline = try engine.execute(a, c, query, tables, try writer.functions(a), null);
    var build = try engine.parse(a, "CREATE INDEX people ON frames('fixture.mp4') (yolo(frame, 'person')) WHERE frame_index <= 3");
    build.filename = path;
    const built = try engine.execute(a, c, build, tables, try writer.functions(a), null);
    try std.testing.expectEqual(@as(i128, @intCast(baseline.rows.len)), built.rows[0][1].int);
    var reader = try video.Runtime.init(std.testing.allocator, io);
    defer reader.deinit();
    reader.person_indexes.directory = writer.person_indexes.directory;
    const indexed = try engine.execute(a, c, query, tables, try reader.functions(a), null);
    try std.testing.expectEqual(baseline.rows.len, indexed.rows.len);
    for (baseline.rows, indexed.rows) |before, after| try std.testing.expectEqual(before[1].int, after[1].int);
    try std.testing.expect(reader.detector == null);
    const missing = try engine.parse(a, "SELECT yolo(frame, 'person') AS people FROM frames WHERE frame_index = 4 LIMIT 1");
    _ = try engine.execute(a, c, missing, tables, try reader.functions(a), null);
    try std.testing.expect(reader.detector != null);
}
