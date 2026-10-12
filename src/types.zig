const std = @import("std");
pub const Allocator = std.mem.Allocator;
pub const Frame = struct {
    source: []const u8,
    track_id: u32,
    frame_index: usize,
    offset: u64,
    size: u32,
    pts_seconds: f64,
    duration_seconds: f64,
    decode_start: f64 = 0,
    seekable: bool = false,
    keyframe: bool = false,
    id: ?[]const u8 = null,

    pub fn jsonStringify(f: Frame, j: anytype) !void {
        try j.beginObject();
        try j.objectField("type");
        try j.write("frame");
        inline for (.{ "source", "track_id", "frame_index", "offset", "size", "pts_seconds", "duration_seconds" }) |field| {
            try j.objectField(field);
            try j.write(@field(f, field));
        }
        if (f.id) |id| {
            try j.objectField("id");
            try j.write(id);
            var buf: [256]u8 = undefined;
            try j.objectField("preview_url");
            try j.write((std.fmt.bufPrint(&buf, "/api/frame/{s}/preview", .{id}) catch unreachable));
            try j.objectField("raw_url");
            try j.write((std.fmt.bufPrint(&buf, "/api/frame/{s}/raw", .{id}) catch unreachable));
        }
        try j.endObject();
    }
};
pub const Value = union(enum) {
    null,
    int: i128,
    float: f64,
    boolean: bool,
    string: []const u8,
    frame: Frame,
    json: []const u8,
    pub fn jsonStringify(v: Value, j: anytype) !void {
        switch (v) {
            .null => try j.write(null),
            .int => |x| try j.write(x),
            .float => |x| try j.write(x),
            .boolean => |x| try j.write(x),
            .string => |x| try j.write(x),
            .frame => |x| try j.write(x),
            .json => |x| {
                try j.beginWriteRaw();
                try j.writer.writeAll(x);
                j.endWriteRaw();
            },
        }
    }
    pub fn number(v: Value) ?f128 {
        return switch (v) {
            .int => |x| @floatFromInt(x),
            .float => |x| @floatCast(x),
            else => null,
        };
    }
};
pub const Table = struct {
    columns: []const []const u8,
    count: usize,
    context: *const anyopaque,
    get: *const fn (*const anyopaque, usize, usize) Value,
    pub const Iterator = struct {
        table: Table,
        order: ?[]const usize,
        position: usize = 0,
        pub fn next(it: *Iterator, control: Control) !?usize {
            try control.check();
            const count = if (it.order) |rows| rows.len else it.table.count;
            if (it.position == count) return null;
            const row = if (it.order) |rows| rows[it.position] else it.position;
            it.position += 1;
            return row;
        }
    };
    /// Rows remain lazy; an ordering stores row indices, never decoded frames.
    pub fn iterator(table: Table, order: ?[]const usize) Iterator {
        return .{ .table = table, .order = order };
    }
    pub fn value(t: Table, row: usize, column: []const u8) Value {
        for (t.columns, 0..) |c, i| {
            if (std.mem.eql(u8, c, column)) return t.get(t.context, row, i);
        }
        return .null;
    }
    pub fn has(t: Table, column: []const u8) bool {
        for (t.columns) |c| {
            if (std.mem.eql(u8, c, column)) return true;
        }
        return false;
    }
};
pub const Result = struct {
    columns: []const []const u8,
    rows: []const []const Value,
    pub fn jsonStringify(r: Result, j: anytype) !void {
        try j.beginObject();
        try j.objectField("columns");
        try j.write(r.columns);
        try j.objectField("rows");
        try j.beginArray();
        for (r.rows) |row| {
            try j.beginObject();
            for (r.columns, row) |c, v| {
                try j.objectField(c);
                try j.write(v);
            }
            try j.endObject();
        }
        try j.endArray();
        try j.endObject();
    }
    pub fn table(r: *const Result) Table {
        return .{ .columns = r.columns, .count = r.rows.len, .context = r, .get = get };
    }
    fn get(ctx: *const anyopaque, row: usize, col: usize) Value {
        const r: *const Result = @ptrCast(@alignCast(ctx));
        return r.rows[row][col];
    }
};
pub const Tables = std.StringHashMap(Table);
pub fn combine(a: Allocator, tables: []const Table) !Table {
    const Combined = struct {
        tables: []const Table,
        fn get(ctx: *const anyopaque, row: usize, col: usize) Value {
            const self: *const @This() = @ptrCast(@alignCast(ctx));
            var n = row;
            for (self.tables) |t| {
                if (n < t.count) return t.get(t.context, n, col);
                n -= t.count;
            }
            return .null;
        }
    };
    const c = try a.create(Combined);
    c.* = .{ .tables = try a.dupe(Table, tables) };
    var count: usize = 0;
    for (tables) |t| count += t.count;
    return .{ .columns = if (tables.len > 0) tables[0].columns else &.{}, .count = count, .context = c, .get = Combined.get };
}

pub const Control = struct {
    io: std.Io,
    check_context: ?*anyopaque = null,
    check_fn: ?*const fn (*anyopaque) bool = null,
    pub fn check(c: Control) !void {
        try c.io.checkCancel();
        if (c.check_fn) |f| {
            if (f(c.check_context.?)) return error.Canceled;
        }
    }
};
pub fn readAt(file: std.Io.File, io: std.Io, bytes: []u8, offset: u64) !void {
    if (try file.readPositionalAll(io, bytes, offset) != bytes.len) return error.TruncatedFile;
}
pub fn env(name: [:0]const u8) ?[]const u8 {
    const p = std.c.getenv(name) orelse return null;
    return std.mem.span(p);
}

test "structured function results preserve JSON state in rows" {
    const mask = Value{ .json = "{\"count\":1,\"masks\":[{\"score\":0.9,\"rle\":[0,2]}]}" };
    const rows = [_][]const Value{ &.{ .{ .int = 3000 }, mask, .{ .boolean = true } }, &.{ .{ .int = 3001 }, .{ .json = "{\"count\":0,\"masks\":[]}" }, .{ .boolean = false } } };
    const result = Result{ .columns = &.{ "frame_index", "segmentation", "after_mask" }, .rows = &rows };
    inline for (.{ std.json.Stringify.Options{}, std.json.Stringify.Options{ .whitespace = .indent_2 } }) |options| {
        const encoded = try std.json.Stringify.valueAlloc(std.testing.allocator, result, options);
        defer std.testing.allocator.free(encoded);
        const decoded = try std.json.parseFromSlice(std.json.Value, std.testing.allocator, encoded, .{});
        defer decoded.deinit();
        const output = decoded.value.object.get("rows").?.array.items;
        try std.testing.expectEqual(@as(i64, 1), output[0].object.get("segmentation").?.object.get("count").?.integer);
        try std.testing.expect(output[0].object.get("after_mask").?.bool);
        try std.testing.expectEqual(@as(i64, 0), output[1].object.get("segmentation").?.object.get("count").?.integer);
    }
    const last = Result{ .columns = &.{"segmentation"}, .rows = &.{&.{mask}} };
    const encoded = try std.json.Stringify.valueAlloc(std.testing.allocator, last, .{});
    defer std.testing.allocator.free(encoded);
    try std.testing.expectEqualStrings("{\"columns\":[\"segmentation\"],\"rows\":[{\"segmentation\":{\"count\":1,\"masks\":[{\"score\":0.9,\"rle\":[0,2]}]}}]}", encoded);
}
