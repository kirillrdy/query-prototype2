const std = @import("std");
const q = @import("query");
pub fn main(init: std.process.Init) void {
    run(init) catch |err| {
        std.debug.print("bquery: {s}\n", .{@errorName(err)});
        std.process.exit(1);
    };
}
fn run(init: std.process.Init) !void {
    const control = @import("query").control;
    control.install();
    var check = control.RequestControl{ .io = init.io };
    const c = q.types.Control{ .io = init.io, .check_context = &check, .check_fn = control.RequestControl.canceled };
    var arena = std.heap.ArenaAllocator.init(init.gpa);
    defer arena.deinit();
    const a = arena.allocator();
    const args = try init.minimal.args.toSlice(a);
    var sql: []const u8 = "SELECT * FROM frames LIMIT 100";
    var i: usize = 1;
    while (i < args.len and std.mem.startsWith(u8, args[i], "-")) {
        if (std.mem.eql(u8, args[i], "--")) {
            i += 1;
            break;
        }
        if (std.mem.eql(u8, args[i], "-h") or std.mem.eql(u8, args[i], "--help")) {
            std.debug.print("Usage: bquery [-query SQL] FILE [FILE ...]\n", .{});
            return;
        }
        const flag = if (std.mem.startsWith(u8, args[i], "--")) args[i][1..] else args[i];
        if (std.mem.startsWith(u8, flag, "-query=")) {
            sql = flag[7..];
            i += 1;
            continue;
        }
        if (!std.mem.eql(u8, flag, "-query") or i + 1 >= args.len) return error.InvalidArgument;
        sql = args[i + 1];
        i += 2;
    }
    if (i == args.len) return error.InputFileRequired;
    const parsed = try q.engine.parse(a, sql);
    if (!parsed.uses(&.{ "person_count", "object_count", "sam3", "sam3_count", "sam3_score" })) {
        check.deadline_ns = std.Io.Timestamp.now(init.io, .awake).nanoseconds + 120 * std.time.ns_per_s;
    }
    var tables = q.types.Tables.init(a);
    var parts: std.ArrayList(q.types.Table) = .empty;
    for (args[i..]) |path| {
        const all = try q.mp4.read(a, c, path, path, parsed.source());
        try parts.append(a, all.get(parsed.source()) orelse return error.UnknownTable);
    }
    try tables.put(parsed.source(), try q.types.combine(a, parts.items));
    var video = try q.video.Runtime.init(init.gpa, init.io);
    defer video.deinit();
    const result = try q.engine.execute(a, c, parsed, tables, try video.functions(a), null);
    const json = try std.json.Stringify.valueAlloc(a, result, .{ .whitespace = .indent_2 });
    try std.Io.File.stdout().writeStreamingAll(init.io, json);
    try std.Io.File.stdout().writeStreamingAll(init.io, "\n");
}
