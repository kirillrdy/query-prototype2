const std = @import("std");
const t = @import("types.zig");
const A = std.mem.Allocator;

// SQL terminators inside quoted literals or comments belong to the SQL text.
pub fn statementEnd(sql: []const u8) ?usize {
    var quote: u8 = 0;
    var comment: usize = 0;
    var line = false;
    var i: usize = 0;
    while (i < sql.len) : (i += 1) {
        const c = sql[i];
        const next: u8 = if (i + 1 < sql.len) sql[i + 1] else 0;
        if (line) {
            if (c == '\n') line = false;
            continue;
        }
        if (comment > 0) {
            if (c == '/' and next == '*') {
                comment += 1;
                i += 1;
            } else if (c == '*' and next == '/') {
                comment -= 1;
                i += 1;
            }
            continue;
        }
        if (quote != 0) {
            if (c == quote) {
                if (next == quote) i += 1 else quote = 0;
            }
        } else if (c == '\'' or c == '"') quote = c else if (c == '-' and next == '-') {
            line = true;
            i += 1;
        } else if (c == '/' and next == '*') {
            comment = 1;
            i += 1;
        } else if (c == ';') return i;
    }
    return null;
}

fn cell(a: A, value: t.Value) ![]const u8 {
    const raw = switch (value) {
        .null => "NULL",
        .int => |v| try std.fmt.allocPrint(a, "{d}", .{v}),
        .float => |v| try std.fmt.allocPrint(a, "{d:.6}", .{v}),
        .boolean => |v| if (v) "true" else "false",
        .string, .json => |v| v,
        .frame => |v| try std.fmt.allocPrint(a, "Frame {d}", .{v.frame_index}),
    };
    // Bound large masks and escape terminal controls. JSON mode retains full data.
    const limit = @min(raw.len, 64);
    var end = limit;
    if (end < raw.len) while (end > 0 and raw[end] & 0xc0 == 0x80) {
        end -= 1;
    };
    const out = try a.alloc(u8, end + if (end < raw.len) @as(usize, 3) else 0);
    for (raw[0..end], out[0..end]) |c, *d| d.* = if (c < 32 or c == 127) ' ' else c;
    if (end < raw.len) @memcpy(out[end..], "...");
    return out;
}
fn columnsWide(s: []const u8) usize {
    return std.unicode.utf8CountCodepoints(s) catch s.len;
}
fn appendCell(a: A, out: *std.ArrayList(u8), s: []const u8, width: usize, numeric: bool) !void {
    const pad = width - columnsWide(s);
    try out.append(a, ' ');
    if (numeric) try out.appendNTimes(a, ' ', pad);
    try out.appendSlice(a, s);
    if (!numeric) try out.appendNTimes(a, ' ', pad);
    try out.append(a, ' ');
}
pub const Table = struct {
    columns: []const []const u8,
    cells: []const []const []const u8,
    numeric: []bool,
    widths: []usize,
    pub fn init(a: A, result: t.Result) !Table {
        const cells = try a.alloc([]const []const u8, result.rows.len);
        const columns = try a.alloc([]const u8, result.columns.len);
        const widths = try a.alloc(usize, result.columns.len);
        const numeric = try a.alloc(bool, result.columns.len);
        @memset(numeric, false);
        for (result.columns, 0..) |name, i| {
            columns[i] = try cell(a, .{ .string = name });
            widths[i] = columnsWide(columns[i]);
        }
        for (result.rows, cells) |row, *values| {
            const items = try a.alloc([]const u8, columns.len);
            for (row, 0..) |value, i| {
                items[i] = try cell(a, value);
                widths[i] = @max(widths[i], columnsWide(items[i]));
                numeric[i] = numeric[i] or value == .int or value == .float;
            }
            values.* = items;
        }
        return .{ .columns = columns, .cells = cells, .numeric = numeric, .widths = widths };
    }
    pub fn page(table: Table, a: A, start: usize, end: usize) ![]const u8 {
        var out: std.ArrayList(u8) = .empty;
        for (table.columns, 0..) |name, i| {
            if (i != 0) try out.append(a, '|');
            try appendCell(a, &out, name, table.widths[i], false);
        }
        try out.append(a, '\n');
        for (table.widths, 0..) |width, i| {
            if (i != 0) try out.append(a, '+');
            try out.appendNTimes(a, '-', width + 2);
        }
        try out.append(a, '\n');
        for (table.cells[start..end]) |row| {
            for (row, 0..) |value, i| {
                if (i != 0) try out.append(a, '|');
                try appendCell(a, &out, value, table.widths[i], table.numeric[i]);
            }
            try out.append(a, '\n');
        }
        return out.items;
    }
};

test "SQL statements retain quoted semicolons and escaped quotes" {
    const sql = "SELECT 'it''s;fine' /* ; */ FROM frames('a;b.mp4'); SELECT 1;";
    try std.testing.expectEqual(std.mem.indexOf(u8, sql, "; SELECT").?, statementEnd(sql).?);
    try std.testing.expect(statementEnd("SELECT 'unfinished;") == null);
    try std.testing.expect(statementEnd("SELECT 1 -- ;\n") == null);
}
test "table pages format frames, nulls and controls without exposing full masks" {
    var arena = std.heap.ArenaAllocator.init(std.testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const result = t.Result{ .columns = &.{ "name", "value" }, .rows = &.{ &.{ .{ .string = "hi\nthere" }, .{ .int = 123 } }, &.{ .{ .string = "next" }, .null } } };
    const table = try Table.init(a, result);
    const first = try table.page(a, 0, 1);
    try std.testing.expect(std.mem.indexOf(u8, first, "hi there") != null);
    try std.testing.expect(std.mem.indexOf(u8, first, "next") == null);
    try std.testing.expect(std.mem.indexOf(u8, try table.page(a, 1, 2), "NULL") != null);
    try std.testing.expectEqualStrings("Frame 48", try cell(a, .{ .frame = .{ .source = "video.mp4", .track_id = 1, .frame_index = 48, .offset = 0, .size = 1, .pts_seconds = 2, .duration_seconds = 0.04 } }));
    const mask: [256]u8 = @splat('1');
    const clipped = try cell(a, .{ .json = &mask });
    try std.testing.expectEqual(@as(usize, 67), clipped.len);
    try std.testing.expect(std.mem.endsWith(u8, clipped, "..."));
}
