// Test-only JSON table input for comparison with the archived Go engine.
const std = @import("std");
const q = @import("query");
const parse_mod = q.parse;
pub fn main(init: std.process.Init) void {
    run(init) catch |err| {
        std.debug.print("{s}\n", .{@errorName(err)});
        std.process.exit(1);
    };
}
fn run(init: std.process.Init) !void {
    var arena = std.heap.ArenaAllocator.init(init.gpa);
    defer arena.deinit();
    const a = arena.allocator();
    const args = try init.minimal.args.toSlice(a);
    const bytes = try std.Io.Dir.cwd().readFileAlloc(init.io, args[1], a, .limited(16 << 20));
    const input = try std.json.parseFromSlice(struct { sql: []const u8, columns: []const []const u8, rows: []const std.json.Value }, a, bytes, .{ .parse_numbers = false });
    const data = input.value;
    const rows = try a.alloc([]const q.types.Value, data.rows.len);
    for (data.rows, 0..) |row, i| {
        const values = try a.alloc(q.types.Value, data.columns.len);
        for (data.columns, 0..) |col, j| values[j] = try convert(a, row.object.get(col) orelse .null);
        rows[i] = values;
    }
    const table = q.types.Result{ .columns = data.columns, .rows = rows };
    var tables = q.types.Tables.init(a);
    try tables.put("frames", table.table());
    const parsed = try q.engine.parse(a, data.sql);
    const out = try q.engine.execute(a, .{ .io = init.io }, parsed, tables, q.engine.Functions.init(a), null);
    try std.Io.File.stdout().writeStreamingAll(init.io, try std.json.Stringify.valueAlloc(a, out, .{}));
}
fn convert(a: std.mem.Allocator, v: std.json.Value) !q.types.Value {
    return switch (v) {
        .null => .null,
        .bool => |b| .{ .boolean = b },
        .string => |s| .{ .string = s },
        .integer => |n| .{ .int = n },
        .float => |n| .{ .float = n },
        .number_string => |s| try parse_mod.number(s),
        else => .{ .json = try std.json.Stringify.valueAlloc(a, v, .{}) },
    };
}
