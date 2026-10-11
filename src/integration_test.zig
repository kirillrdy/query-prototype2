const std = @import("std");
const t = @import("types.zig");
const mp4 = @import("mp4.zig");
const engine = @import("engine.zig");
const video = @import("video.zig");
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
test "CPU object detection runs without a SAM3 worker" {
    const path = t.env("BQUERY_TEST_DETECTION_VIDEO") orelse return error.SkipZigTest;
    var arena = std.heap.ArenaAllocator.init(std.testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const io = std.testing.io;
    const c = t.Control{ .io = io };
    const tables = try mp4.read(a, c, path, path, "frames");
    var runtime = try video.Runtime.init(std.testing.allocator, io);
    defer runtime.deinit();
    const query = try engine.parse(a, "SELECT person_count(frame) AS people, object_count(frame, 'car') AS cars FROM frames WHERE is_keyframe = true LIMIT 1");
    const result = try engine.execute(a, c, query, tables, try runtime.functions(a), null);
    try std.testing.expectEqual(@as(usize, 1), result.rows.len);
    try std.testing.expect(result.rows[0][0].int >= 0 and result.rows[0][1].int >= 0);
}
