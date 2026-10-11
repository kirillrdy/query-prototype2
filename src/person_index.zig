const std = @import("std");
const t = @import("types.zig");
const A = std.mem.Allocator;
const Key = struct { track: u32, frame: usize };
const Entry = struct { track: u32, frame: usize, people: u32 };
const Document = struct { version: u32 = 1, name: []const u8, fingerprint: []const u8, entries: []const Entry };
const Bucket = std.AutoHashMap(Key, u32);
const Cache = struct { values: Bucket, stamp: ?i96 };
pub const Store = struct {
    a: A,
    io: std.Io,
    buckets: std.StringHashMap(*Cache),
    directory: ?[]const u8 = null,
    pub fn init(a: A, io: std.Io) Store {
        return .{ .a = a, .io = io, .buckets = .init(a) };
    }
    pub fn deinit(s: *Store) void {
        var it = s.buckets.iterator();
        while (it.next()) |e| {
            s.a.free(e.key_ptr.*);
            e.value_ptr.*.values.deinit();
            s.a.destroy(e.value_ptr.*);
        }
        s.buckets.deinit();
    }
    pub fn root(a: A) ![]const u8 {
        return t.env("BQUERY_INDEX_CACHE") orelse try std.fmt.allocPrint(a, "{s}/binaryquery/indexes", .{t.env("XDG_CACHE_HOME") orelse try std.fmt.allocPrint(a, "{s}/.cache", .{t.env("HOME") orelse return error.HomeNotSet})});
    }
    fn getRoot(s: *Store, a: A) ![]const u8 {
        return s.directory orelse try root(a);
    }
    pub fn digest(a: A, bytes: []const u8) ![]const u8 {
        var hash: [32]u8 = undefined;
        std.crypto.hash.sha2.Sha256.hash(bytes, &hash, .{});
        return a.dupe(u8, &std.fmt.bytesToHex(hash, .lower));
    }
    pub fn settings(a: A, io: std.Io, threshold: f32) ![]const u8 {
        const model = if (t.env("BQUERY_DETECTION_MODEL")) |path| blk: {
            const abs = try std.Io.Dir.cwd().realPathFileAlloc(io, path, a);
            const stat = try std.Io.Dir.cwd().statFile(io, abs, .{});
            break :blk try std.fmt.allocPrint(a, "{s}:{d}:{d}", .{ abs, stat.size, stat.mtime.nanoseconds });
        } else "yolo11n:634279b40c07c6391472c51ad45b81ebc48706a9a1fe72dd3396322acd0c053b";
        return digest(a, try std.fmt.allocPrint(a, "person-index-v1:ort-1.23.2:letterbox-640:nms-v1:{s}:{d}", .{ model, @as(u32, @bitCast(threshold)) }));
    }
    pub fn identity(s: *Store, a: A, source: []const u8, config: []const u8) ![]const u8 {
        const abs = try std.Io.Dir.cwd().realPathFileAlloc(s.io, source, a);
        const stat = try std.Io.Dir.cwd().statFile(s.io, abs, .{});
        return digest(a, try std.fmt.allocPrint(a, "{s}:{d}:{d}:{s}", .{ abs, stat.size, stat.mtime.nanoseconds, config }));
    }
    fn sourcePrefix(s: *Store, a: A, source: []const u8) ![]const u8 {
        return digest(a, try std.Io.Dir.cwd().realPathFileAlloc(s.io, source, a));
    }
    fn bucket(s: *Store, a: A, c: t.Control, source: []const u8, fingerprint: []const u8) !*Bucket {
        const directory = try s.getRoot(a);
        const stat = std.Io.Dir.cwd().statFile(s.io, directory, .{}) catch |err| switch (err) {
            error.FileNotFound => null,
            else => return err,
        };
        const stamp: ?i96 = if (stat) |v| v.mtime.nanoseconds else null;
        if (s.buckets.get(fingerprint)) |b| {
            if (b.stamp == stamp) return &b.values;
            const old = s.buckets.fetchRemove(fingerprint).?;
            s.a.free(old.key);
            old.value.values.deinit();
            s.a.destroy(old.value);
        }
        const b = try s.a.create(Cache);
        b.* = .{ .values = .init(s.a), .stamp = stamp };
        errdefer {
            b.values.deinit();
            s.a.destroy(b);
        }
        const prefix = try s.sourcePrefix(a, source);
        const dir = std.Io.Dir.cwd().openDir(s.io, directory, .{ .iterate = true }) catch |err| switch (err) {
            error.FileNotFound => null,
            else => return err,
        };
        if (dir) |d| {
            defer d.close(s.io);
            var it = d.iterate();
            while (try it.next(s.io)) |entry| {
                try c.check();
                if (entry.kind != .file or !std.mem.startsWith(u8, entry.name, prefix) or !std.mem.endsWith(u8, entry.name, ".json")) continue;
                // Each file has its own arena so scanning indexes stays bounded.
                var arena = std.heap.ArenaAllocator.init(a);
                defer arena.deinit();
                const alloc = arena.allocator();
                const bytes = d.readFileAlloc(s.io, entry.name, alloc, .limited(64 << 20)) catch continue;
                const doc = std.json.parseFromSlice(Document, alloc, bytes, .{}) catch continue;
                if (doc.value.version != 1 or !std.mem.eql(u8, doc.value.fingerprint, fingerprint)) continue;
                for (doc.value.entries) |value| try b.values.put(.{ .track = value.track, .frame = value.frame }, value.people);
            }
        }
        const key = try s.a.dupe(u8, fingerprint);
        errdefer s.a.free(key);
        try s.buckets.put(key, b);
        return &b.values;
    }
    pub fn lookup(s: *Store, a: A, c: t.Control, f: t.Frame, config: []const u8) !?u32 {
        const fingerprint = try s.identity(a, f.source, config);
        return (try s.bucket(a, c, f.source, fingerprint)).get(.{ .track = f.track_id, .frame = f.frame_index });
    }
    pub fn save(s: *Store, a: A, c: t.Control, name: []const u8, source: []const u8, config: []const u8, result: t.Result) !void {
        try c.check();
        const fingerprint = try s.identity(a, source, config);
        const entries = try a.alloc(Entry, result.rows.len);
        for (result.rows, entries) |row, *entry| {
            if (row.len != 2 or row[0] != .frame or row[1] != .int or row[1].int < 0 or row[1].int > std.math.maxInt(u32)) return error.InvalidIndexResult;
            if (!std.mem.eql(u8, fingerprint, try s.identity(a, row[0].frame.source, config))) return error.InvalidIndexSource;
            entry.* = .{ .track = row[0].frame.track_id, .frame = row[0].frame.frame_index, .people = @intCast(row[1].int) };
        }
        const directory = try s.getRoot(a);
        try std.Io.Dir.cwd().createDirPath(s.io, directory);
        const path = try std.fmt.allocPrint(a, "{s}/{s}-{s}.json", .{ directory, try s.sourcePrefix(a, source), try digest(a, name) });
        var random: [16]u8 = undefined;
        s.io.random(&random);
        const staging = try std.fmt.allocPrint(a, "{s}.{s}.part", .{ path, std.fmt.bytesToHex(random, .lower) });
        defer std.Io.Dir.cwd().deleteFile(s.io, staging) catch {};
        const bytes = try std.json.Stringify.valueAlloc(a, Document{ .name = name, .fingerprint = fingerprint, .entries = entries }, .{});
        if (bytes.len > 64 << 20) return error.IndexTooLarge;
        const file = try std.Io.Dir.cwd().createFile(s.io, staging, .{ .exclusive = true });
        defer file.close(s.io);
        try file.writeStreamingAll(s.io, bytes);
        try file.sync(s.io);
        try c.check();
        if (!std.mem.eql(u8, fingerprint, try s.identity(a, source, config))) return error.SourceChanged;
        try std.Io.Dir.cwd().rename(staging, .cwd(), path, s.io);
        // Refresh merged partial indexes, including when replacing a named index.
        if (s.buckets.fetchRemove(fingerprint)) |old| {
            s.a.free(old.key);
            old.value.values.deinit();
            s.a.destroy(old.value);
        }
    }
};

test "persistent counts reload across sessions and reject changed source or settings" {
    var arena = std.heap.ArenaAllocator.init(std.testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const io = std.testing.io;
    var random: [16]u8 = undefined;
    io.random(&random);
    const directory = try std.fmt.allocPrint(a, "/tmp/bquery-index-test-{s}", .{std.fmt.bytesToHex(random, .lower)});
    try std.Io.Dir.cwd().createDirPath(io, directory);
    defer std.Io.Dir.cwd().deleteTree(io, directory) catch {};
    const source = try std.fmt.allocPrint(a, "{s}/source.mp4", .{directory});
    const file = try std.Io.Dir.cwd().createFile(io, source, .{});
    try file.writeStreamingAll(io, "fixture");
    file.close(io);
    const frame = t.Frame{ .source = source, .track_id = 1, .frame_index = 1, .offset = 0, .size = 1, .pts_seconds = 0, .duration_seconds = 1 };
    var second = frame;
    second.track_id = 2;
    const result = t.Result{ .columns = &.{ "frame", "people" }, .rows = &.{ &.{ .{ .frame = frame }, .{ .int = 2 } }, &.{ .{ .frame = second }, .{ .int = 0 } } } };
    var writer = Store.init(std.testing.allocator, io);
    defer writer.deinit();
    writer.directory = try std.fmt.allocPrint(a, "{s}/indexes", .{directory});
    var reader = Store.init(std.testing.allocator, io);
    defer reader.deinit();
    reader.directory = writer.directory;
    const c = t.Control{ .io = io };
    try std.testing.expect(try reader.lookup(a, c, frame, "model-a:confidence-a") == null);
    try writer.save(a, c, "people", source, "model-a:confidence-a", result);
    // A session that already cached a miss notices a newly published index.
    try std.testing.expectEqual(@as(?u32, 2), try reader.lookup(a, c, frame, "model-a:confidence-a"));
    try std.testing.expectEqual(@as(?u32, 0), try reader.lookup(a, c, second, "model-a:confidence-a"));
    var outside = frame;
    outside.frame_index = 3;
    try std.testing.expect(try reader.lookup(a, c, outside, "model-a:confidence-a") == null);
    try std.testing.expect(try reader.lookup(a, c, frame, "model-b:confidence-a") == null);
    try std.testing.expect(try reader.lookup(a, c, frame, "model-a:confidence-b") == null);
    var fresh = Store.init(std.testing.allocator, io);
    defer fresh.deinit();
    fresh.directory = writer.directory;
    try std.testing.expectEqual(@as(?u32, 2), try fresh.lookup(a, c, frame, "model-a:confidence-a"));
    const replacement = try std.Io.Dir.cwd().createFile(io, source, .{});
    try replacement.writeStreamingAll(io, "changed fixture size");
    replacement.close(io);
    try std.testing.expect(try reader.lookup(a, c, frame, "model-a:confidence-a") == null);
}
