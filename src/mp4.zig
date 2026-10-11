const std = @import("std");
const t = @import("types.zig");
const A = std.mem.Allocator;
const V = t.Value;
const max_samples = 1_000_000;
pub const schemas = .{
    .files = &[_][]const u8{ "path", "format", "size", "major_brand", "duration_seconds", "track_count" },
    .tracks = &[_][]const u8{ "path", "track_id", "handler", "codec", "width", "height", "timescale", "duration_seconds" },
    .boxes = &[_][]const u8{ "path", "box_path", "type", "offset", "size", "depth" },
    .frames = &[_][]const u8{ "path", "track_id", "frame_index", "dts_seconds", "pts_seconds", "duration_seconds", "size", "is_keyframe", "offset", "frame" },
};
pub fn schema(name: []const u8) ?[]const []const u8 {
    inline for (@typeInfo(@TypeOf(schemas)).@"struct".field_names) |f| {
        if (eq(name, f)) return @field(schemas, f);
    }
    return null;
}
fn eq(a: []const u8, b: []const u8) bool {
    return std.mem.eql(u8, a, b);
}
fn be32(d: []const u8, p: usize) u32 {
    return std.mem.readInt(u32, d[p..][0..4], .big);
}
fn be64(d: []const u8, p: usize) u64 {
    return std.mem.readInt(u64, d[p..][0..8], .big);
}
const Run = struct { count: u32, value: i64 };
const ChunkRun = struct { first: u32, samples: u32, description: u32 };
const Samples = struct {
    seen: std.StringHashMap(void),
    timing: []const Run = &.{},
    composition: []const Run = &.{},
    sizes: []const u32 = &.{},
    sync: ?std.AutoHashMap(u32, void) = null,
    chunks: []const u64 = &.{},
    chunk_runs: []const ChunkRun = &.{},
};
const Track = struct { row: [8]V = @splat(.null), samples: Samples };
const Range = struct { start: u64, end: u64 };
const Box = struct { kind: []const u8, path: []const u8, offset: u64, size: u64, header: u64, depth: usize };
const IndexedTrack = struct {
    path: []const u8,
    id: ?u32,
    scale: f64,
    durations: []const i64,
    composition: []const i64,
    dts: []const i64,
    sizes: []const u32,
    sync: ?std.AutoHashMap(u32, void),
    locations: ?[]const u64,
    anchors: []const ?usize,
    presentation_order: []const usize,
    fn get(ctx: *const anyopaque, row: usize, col: usize) V {
        const s: *const @This() = @ptrCast(@alignCast(ctx));
        const i = s.presentation_order[row];
        const pts = @as(f64, @floatFromInt(s.dts[i] + s.composition[i])) / s.scale;
        const duration = @as(f64, @floatFromInt(s.durations[i])) / s.scale;
        const key = if (s.sync) |sync| sync.contains(@intCast(i + 1)) else true;
        return switch (col) {
            0 => .{ .string = s.path },
            1 => if (s.id) |id| .{ .int = id } else .null,
            2 => .{ .int = row + 1 },
            3 => .{ .float = @as(f64, @floatFromInt(s.dts[i])) / s.scale },
            4 => .{ .float = pts },
            5 => .{ .float = duration },
            6 => .{ .int = s.sizes[i] },
            7 => .{ .boolean = key },
            8 => if (s.locations) |loc| .{ .int = loc[i] } else .null,
            9 => if (s.id != null and s.locations != null) .{ .frame = .{
                .source = s.path,
                .track_id = s.id.?,
                .frame_index = row + 1,
                .offset = s.locations.?[i],
                .size = s.sizes[i],
                .pts_seconds = pts,
                .duration_seconds = duration,
                .keyframe = key,
                .seekable = s.anchors[i] != null,
                .decode_start = if (s.anchors[i]) |n| @as(f64, @floatFromInt(s.dts[n] + s.composition[n])) / s.scale else 0,
            } } else .null,
            else => .null,
        };
    }
};
pub fn probe(file: std.Io.File, io: std.Io) bool {
    var h: [8]u8 = undefined;
    t.readAt(file, io, &h, 0) catch return false;
    for ([_][]const u8{ "ftyp", "moov", "mdat", "free", "wide", "skip" }) |kind| {
        if (eq(h[4..], kind)) return true;
    }
    return false;
}
pub fn read(a: A, control: t.Control, path: []const u8, name: []const u8, table: []const u8) !t.Tables {
    const file = try std.Io.Dir.cwd().openFile(control.io, path, .{});
    defer file.close(control.io);
    const stat = try file.stat(control.io);
    if (stat.kind != .file) return error.NotRegularFile;
    if (!probe(file, control.io)) return error.UnsupportedFormat;
    var p = Parser{ .a = a, .control = control, .file = file, .size = stat.size, .name = name, .read_frames = eq(table, "frames") };
    p.file_row = .{ .{ .string = name }, .{ .string = "mp4" }, .{ .int = stat.size }, .null, .null, .null };
    try p.walk(0, stat.size, "", 0, null);
    if (!p.has_movie) return error.MissingMovieBox;
    p.file_row[5] = .{ .int = p.tracks.items.len };
    var tables = t.Tables.init(a);
    const files = try a.create(t.Result);
    files.* = .{ .columns = schemas.files, .rows = try a.dupe([]const V, &.{try a.dupe(V, &p.file_row)}) };
    try tables.put("files", files.table());
    var rows: std.ArrayList([]const V) = .empty;
    for (p.tracks.items) |track| try rows.append(a, try a.dupe(V, &track.row));
    const tracks = try a.create(t.Result);
    tracks.* = .{ .columns = schemas.tracks, .rows = try rows.toOwnedSlice(a) };
    try tables.put("tracks", tracks.table());
    const boxes = try a.create(t.Result);
    boxes.* = .{ .columns = schemas.boxes, .rows = try p.boxes.toOwnedSlice(a) };
    try tables.put("boxes", boxes.table());
    if (p.read_frames) try tables.put("frames", try p.frames());
    return tables;
}
const Parser = struct {
    a: A,
    control: t.Control,
    file: std.Io.File,
    size: u64,
    name: []const u8,
    read_frames: bool,
    boxes: std.ArrayList([]const V) = .empty,
    tracks: std.ArrayList(*Track) = .empty,
    media: std.ArrayList(Range) = .empty,
    file_row: [6]V = @splat(.null),
    has_movie: bool = false,
    fn bytes(p: *Parser, offset: u64, size: u64) ![]u8 {
        if (offset > p.size or size > p.size - offset) return error.ReadOutsideFile;
        const d = try p.a.alloc(u8, @intCast(size));
        try t.readAt(p.file, p.control.io, d, offset);
        return d;
    }
    fn walk(p: *Parser, start: u64, end: u64, parent: []const u8, depth: usize, track: ?*Track) anyerror!void {
        if (depth > 32) return error.BoxNestingLimit;
        var off = start;
        while (off < end) {
            try p.control.check();
            if (p.boxes.items.len >= 100000) return error.BoxCountLimit;
            if (end - off < 8) return error.TruncatedBoxHeader;
            const h = try p.bytes(off, 8);
            var size: u64 = be32(h, 0);
            var header: u64 = 8;
            const kind = h[4..8];
            if (size == 1) {
                if (end - off < 16) return error.TruncatedBoxHeader;
                size = be64(try p.bytes(off + 8, 8), 0);
                header = 16;
            }
            if (size == 0) size = end - off;
            if (eq(kind, "uuid")) header += 16;
            if (size < header or size > end - off or size > std.math.maxInt(i64)) return error.InvalidBoxSize;
            const b = Box{ .kind = kind, .path = try std.fmt.allocPrint(p.a, "{s}/{s}", .{ parent, kind }), .offset = off, .size = size, .header = header, .depth = depth };
            try p.boxes.append(p.a, try p.a.dupe(V, &.{ .{ .string = p.name }, .{ .string = b.path }, .{ .string = kind }, .{ .int = off }, .{ .int = size }, .{ .int = depth } }));
            if (eq(kind, "mdat")) try p.media.append(p.a, .{ .start = off + header, .end = off + size });
            if (eq(kind, "moov") and depth == 0) p.has_movie = true;
            try p.metadata(b, track);
            if (eq(kind, "trak")) {
                const tr = try p.a.create(Track);
                tr.* = .{ .samples = .{ .seen = std.StringHashMap(void).init(p.a) } };
                tr.row[0] = .{ .string = p.name };
                try p.walk(off + header, off + size, b.path, depth + 1, tr);
                try p.tracks.append(p.a, tr);
            } else {
                for ([_][]const u8{ "moov", "mdia", "minf", "stbl", "edts", "mvex", "moof", "traf", "dinf" }) |container| {
                    if (eq(kind, container)) {
                        try p.walk(off + header, off + size, b.path, depth + 1, track);
                        break;
                    }
                }
            }
            off += size;
        }
    }
    fn metadata(p: *Parser, b: Box, tr: ?*Track) !void {
        for ([_][]const u8{ "stts", "ctts", "stsz", "stss", "stsc", "stco", "co64" }) |kind| {
            if (eq(b.kind, kind)) {
                if (tr != null and p.read_frames) try p.samples(b, &tr.?.samples);
                return;
            }
        }
        const n: u64 = if (eq(b.kind, "ftyp")) 8 else if (eq(b.kind, "mvhd") or eq(b.kind, "mdhd")) 32 else if (eq(b.kind, "tkhd")) 96 else if (eq(b.kind, "hdlr")) 12 else if (eq(b.kind, "stsd")) 16 else return;
        const d = try p.bytes(b.offset + b.header, @min(n, b.size - b.header));
        if (eq(b.kind, "ftyp")) {
            if (d.len < 8) return error.TruncatedMetadata;
            p.file_row[3] = .{ .string = d[0..4] };
        }
        if (eq(b.kind, "mvhd") or eq(b.kind, "mdhd")) {
            if (d.len < 4 or d[0] > 1) return error.InvalidFullBoxVersion;
            const pos: usize = if (d[0] == 1) 20 else 12;
            if (d.len < pos + (if (d[0] == 1) @as(usize, 12) else 8)) return error.TruncatedMetadata;
            const scale = be32(d, pos);
            if (scale == 0) return error.ZeroTimescale;
            const duration: u64 = if (d[0] == 1) be64(d, pos + 4) else be32(d, pos + 4);
            const unknown = duration == @as(u64, (if (d[0] == 1) std.math.maxInt(u64) else std.math.maxInt(u32)));
            if (eq(b.kind, "mvhd")) {
                if (!unknown) p.file_row[4] = .{ .float = @as(f64, @floatFromInt(duration)) / @as(f64, @floatFromInt(scale)) };
            } else if (tr) |track| {
                track.row[6] = .{ .int = scale };
                if (!unknown) track.row[7] = .{ .float = @as(f64, @floatFromInt(duration)) / @as(f64, @floatFromInt(scale)) };
            }
        }
        if (eq(b.kind, "tkhd") and tr != null) {
            if (d.len < 4 or d[0] > 1) return error.InvalidFullBoxVersion;
            const id: usize = if (d[0] == 1) 20 else 12;
            const width: usize = if (d[0] == 1) 88 else 76;
            if (d.len < width + 8) return error.TruncatedMetadata;
            tr.?.row[1] = .{ .int = be32(d, id) };
            tr.?.row[4] = .{ .float = @as(f64, @floatFromInt(be32(d, width))) / 65536 };
            tr.?.row[5] = .{ .float = @as(f64, @floatFromInt(be32(d, width + 4))) / 65536 };
        }
        if (eq(b.kind, "hdlr")) {
            if (d.len < 12) return error.TruncatedMetadata;
            if (tr) |track| track.row[2] = .{ .string = d[8..12] };
        }
        if (eq(b.kind, "stsd")) {
            if (d.len < 8) return error.TruncatedMetadata;
            if (be32(d, 4) == 0) return;
            if (d.len < 16) return error.TruncatedMetadata;
            const entry = be32(d, 8);
            if (entry < 8 or entry > b.size - b.header - 8) return error.InvalidSampleDescription;
            if (tr) |track| track.row[3] = .{ .string = d[12..16] };
        }
    }
    fn samples(p: *Parser, b: Box, s: *Samples) !void {
        if (s.seen.contains(b.kind)) return error.DuplicateSampleTable;
        try s.seen.put(b.kind, {});
        const available = b.size - b.header;
        if (available < 8) return error.TruncatedSampleTable;
        const h = try p.bytes(b.offset + b.header, 8);
        if (h[0] > (if (eq(b.kind, "ctts")) @as(u8, 1) else 0)) return error.InvalidFullBoxVersion;
        var count = be32(h, 4);
        var entry: usize = 8;
        var header: u64 = 8;
        var default_size: u32 = 0;
        if (eq(b.kind, "stsz")) {
            if (available < 12) return error.TruncatedSampleTable;
            default_size = count;
            count = be32(try p.bytes(b.offset + b.header + 8, 4), 0);
            entry = 4;
            header = 12;
        }
        if (eq(b.kind, "stss") or eq(b.kind, "stco")) entry = 4;
        if (eq(b.kind, "stsc")) entry = 12;
        if (count > max_samples) return error.SampleCountLimit;
        const size: u64 = if (eq(b.kind, "stsz") and default_size != 0) 0 else @as(u64, count) * entry;
        if (size > available - header) return error.TruncatedSampleTable;
        const d = try p.bytes(b.offset + b.header + header, size);
        var sizes: []u32 = &.{};
        if (eq(b.kind, "stsz")) {
            sizes = try p.a.alloc(u32, count);
            s.sizes = sizes;
        }
        if (eq(b.kind, "stss")) s.sync = std.AutoHashMap(u32, void).init(p.a);
        var runs: std.ArrayList(Run) = .empty;
        var chunks: std.ArrayList(u64) = .empty;
        var chunk_runs: std.ArrayList(ChunkRun) = .empty;
        var total: u64 = 0;
        for (0..count) |i| {
            if (i % 1024 == 0) try p.control.check();
            const off = i * entry;
            if (eq(b.kind, "stsz")) sizes[i] = if (default_size != 0) default_size else be32(d, off) else if (eq(b.kind, "stss")) {
                const index = be32(d, off);
                if (index == 0 or s.sync.?.contains(index)) return error.InvalidSyncSample;
                try s.sync.?.put(index, {});
            } else if (eq(b.kind, "stco") or eq(b.kind, "co64")) try chunks.append(p.a, if (eq(b.kind, "stco")) be32(d, off) else be64(d, off)) else if (eq(b.kind, "stsc")) {
                const r = ChunkRun{ .first = be32(d, off), .samples = be32(d, off + 4), .description = be32(d, off + 8) };
                if (r.first == 0 or r.samples == 0 or r.description == 0 or (chunk_runs.items.len == 0 and r.first != 1) or (chunk_runs.items.len > 0 and r.first <= chunk_runs.items[chunk_runs.items.len - 1].first)) return error.InvalidChunkMapping;
                try chunk_runs.append(p.a, r);
            } else {
                const n = be32(d, off);
                var value: i64 = be32(d, off + 4);
                if (eq(b.kind, "ctts") and h[0] == 1) value = @as(i32, @bitCast(be32(d, off + 4)));
                total += n;
                if (total > max_samples) return error.SampleCountLimit;
                try runs.append(p.a, .{ .count = n, .value = value });
            }
        }
        if (eq(b.kind, "stts")) s.timing = try runs.toOwnedSlice(p.a);
        if (eq(b.kind, "ctts")) s.composition = try runs.toOwnedSlice(p.a);
        if (eq(b.kind, "stco") or eq(b.kind, "co64")) s.chunks = try chunks.toOwnedSlice(p.a);
        if (eq(b.kind, "stsc")) s.chunk_runs = try chunk_runs.toOwnedSlice(p.a);
    }
    fn locations(p: *Parser, s: *Samples) !?[]const u64 {
        const sc = s.seen.contains("stsc");
        const co = s.seen.contains("stco");
        const co64 = s.seen.contains("co64");
        if (!sc and !co and !co64) return null;
        if (!sc or (!co and !co64) or (co and co64)) return error.IncompleteSampleLocations;
        if (s.sizes.len == 0) {
            if (s.chunks.len != 0 or s.chunk_runs.len != 0) return error.InvalidChunkMapping;
            return &.{};
        }
        if (s.chunk_runs.len == 0 or s.chunks.len == 0 or s.chunk_runs[s.chunk_runs.len - 1].first > s.chunks.len) return error.InvalidChunkMapping;
        const result = try p.a.alloc(u64, s.sizes.len);
        var n: usize = 0;
        var run: usize = 0;
        for (s.chunks, 0..) |chunk, i| {
            try p.control.check();
            while (run + 1 < s.chunk_runs.len and s.chunk_runs[run + 1].first <= i + 1) run += 1;
            const count = s.chunk_runs[run].samples;
            if (count > result.len - n) return error.SampleCountMismatch;
            var pos = chunk;
            for (0..count) |_| {
                const size = s.sizes[n];
                if (pos > p.size or size > p.size - pos) return error.SampleOutsideSource;
                var valid = false;
                for (p.media.items) |range| {
                    if (pos >= range.start and pos < range.end and size <= range.end - pos) {
                        valid = true;
                        break;
                    }
                }
                if (!valid) return error.SampleOutsideMedia;
                result[n] = pos;
                n += 1;
                pos += size;
            }
        }
        if (n != result.len) return error.SampleCountMismatch;
        return result;
    }
    fn frames(p: *Parser) !t.Table {
        var tables: std.ArrayList(t.Table) = .empty;
        var total: usize = 0;
        for (p.tracks.items) |track| {
            if (track.row[2] != .string or !eq(track.row[2].string, "vide")) continue;
            const s = &track.samples;
            const sz = s.seen.contains("stsz");
            const ts = s.seen.contains("stts");
            if (!sz and !ts) continue;
            if (!sz or !ts) return error.MissingSampleTables;
            const n = s.sizes.len;
            total += n;
            if (total > max_samples) return error.SampleCountLimit;
            const loc = try p.locations(s);
            const durations = try expand(p.a, s.timing, n);
            const composition = if (s.seen.contains("ctts")) try expand(p.a, s.composition, n) else blk: {
                const c = try p.a.alloc(i64, n);
                @memset(c, 0);
                break :blk c;
            };
            if (s.sync) |sync| {
                var it = sync.keyIterator();
                while (it.next()) |index| {
                    if (index.* > n) return error.InvalidSyncSample;
                }
            }
            if (n == 0) continue;
            if (track.row[6] != .int or track.row[6].int == 0) return error.ZeroTimescale;
            const dts = try p.a.alloc(i64, n);
            dts[0] = 0;
            for (1..n) |i| dts[i] = dts[i - 1] + durations[i - 1];
            const anchors = try p.a.alloc(?usize, n);
            var anchor: ?usize = null;
            var previous: ?usize = null;
            for (anchors, 0..) |*a, i| {
                if (s.sync == null or s.sync.?.contains(@intCast(i + 1))) {
                    previous = anchor;
                    anchor = i;
                }
                a.* = if (anchor != i and previous != null) previous else anchor;
            }
            const indexed = try p.a.create(IndexedTrack);
            const presentation_order = try presentationOrder(p.a, p.control, dts, composition);
            indexed.* = .{ .path = p.name, .id = if (track.row[1] == .int) @intCast(track.row[1].int) else null, .scale = @floatFromInt(track.row[6].int), .durations = durations, .composition = composition, .dts = dts, .sizes = s.sizes, .sync = s.sync, .locations = loc, .anchors = anchors, .presentation_order = presentation_order };
            try tables.append(p.a, .{ .columns = schemas.frames, .count = n, .context = indexed, .get = IndexedTrack.get });
        }
        if (tables.items.len == 0) {
            const empty = try p.a.create(t.Result);
            empty.* = .{ .columns = schemas.frames, .rows = &.{} };
            return empty.table();
        }
        return t.combine(p.a, tables.items);
    }
};
fn presentationOrder(a: A, control: t.Control, dts: []const i64, composition: []const i64) ![]const usize {
    const order = try a.alloc(usize, dts.len);
    errdefer a.free(order);
    var sorted = true;
    for (order, 0..) |*sample, i| {
        if (i % 1024 == 0) try control.check();
        sample.* = i;
        if (i > 0 and dts[i] + composition[i] < dts[i - 1] + composition[i - 1]) sorted = false;
    }
    if (!sorted) {
        const Context = struct {
            dts: []const i64,
            composition: []const i64,
            fn less(ctx: @This(), lhs: usize, rhs: usize) bool {
                const left = ctx.dts[lhs] + ctx.composition[lhs];
                const right = ctx.dts[rhs] + ctx.composition[rhs];
                return left < right or (left == right and lhs < rhs);
            }
        };
        std.mem.sort(usize, order, Context{ .dts = dts, .composition = composition }, Context.less);
    }
    try control.check();
    return order;
}
fn expand(a: A, runs: []const Run, count: usize) ![]const i64 {
    const result = try a.alloc(i64, count);
    errdefer a.free(result);
    var n: usize = 0;
    for (runs) |r| {
        if (r.count > count - n) return error.SampleCountMismatch;
        @memset(result[n..][0..r.count], r.value);
        n += r.count;
    }
    if (n != count) return error.SampleCountMismatch;
    return result;
}
test "sample expansion checks counts" {
    const a = std.testing.allocator;
    const values = try expand(a, &.{.{ .count = 2, .value = -1 }}, 2);
    defer a.free(values);
    try std.testing.expectEqualSlices(i64, &.{ -1, -1 }, values);
    try std.testing.expectError(error.SampleCountMismatch, expand(std.heap.page_allocator, &.{.{ .count = 3, .value = 1 }}, 2));
}
test "presentation indices retain exact decode sample metadata and anchors" {
    const a = std.testing.allocator;
    const dts = [_]i64{ 0, 1, 2, 3, 4 };
    const composition = [_]i64{ 2, 3, 0, -2, -2 };
    const order = try presentationOrder(a, .{ .io = std.testing.io }, &dts, &composition);
    defer a.free(order);
    try std.testing.expectEqualSlices(usize, &.{ 3, 0, 2, 4, 1 }, order);
    var sync = std.AutoHashMap(u32, void).init(a);
    defer sync.deinit();
    try sync.put(1, {});
    const track = IndexedTrack{
        .path = "fixture.mp4",
        .id = 1,
        .scale = 1,
        .durations = &.{ 1, 2, 3, 4, 5 },
        .composition = &composition,
        .dts = &dts,
        .sizes = &.{ 10, 20, 30, 40, 50 },
        .sync = sync,
        .locations = &.{ 100, 200, 300, 400, 500 },
        .anchors = &.{ 0, 0, 0, 0, 0 },
        .presentation_order = order,
    };
    for (order, 0..) |sample, row| {
        const frame = IndexedTrack.get(&track, row, 9).frame;
        try std.testing.expectEqual(@as(i128, @intCast(row + 1)), IndexedTrack.get(&track, row, 2).int);
        try std.testing.expectEqual(row + 1, frame.frame_index);
        try std.testing.expectEqual(track.locations.?[sample], frame.offset);
        try std.testing.expectEqual(track.sizes[sample], frame.size);
        try std.testing.expectEqual(@as(f64, @floatFromInt(dts[sample])), IndexedTrack.get(&track, row, 3).float);
        try std.testing.expectEqual(@as(f64, @floatFromInt(dts[sample] + composition[sample])), frame.pts_seconds);
        try std.testing.expectEqual(@as(f64, @floatFromInt(track.durations[sample])), frame.duration_seconds);
        try std.testing.expectEqual(sample == 0, frame.keyframe);
        try std.testing.expectEqual(@as(f64, 2), frame.decode_start);
    }
}
test "presentation order without composition offsets matches decode order" {
    const a = std.testing.allocator;
    const order = try presentationOrder(a, .{ .io = std.testing.io }, &.{ 0, 10, 20 }, &.{ 0, 0, 0 });
    defer a.free(order);
    try std.testing.expectEqualSlices(usize, &.{ 0, 1, 2 }, order);
}
