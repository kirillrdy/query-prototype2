const std = @import("std");
const q = @import("query");
const A = std.mem.Allocator;
const help =
    \\Usage: bquery [-query SQL | -c SQL] [--json] [--no-pager] [--page-size N] [FILE ...]
    \\Without a query, starts an SQL REPL. End statements with ; or use \g.
    \\SQL can open files directly: SELECT * FROM frames('video.mp4') LIMIT 48;
    \\Commands: \q quit, \h help, \g execute, \r clear query, \p show query
    \\          \pset pager on|off, \pset format table|json
    \\Tables page in a terminal (24 rows per page); redirected output never pauses.
    \\JSON includes complete frame metadata and mask results.
    \\
;
const Options = struct { json: bool = false, pager: bool = true, page_size: usize = 24 };
pub fn main(init: std.process.Init) void {
    run(init) catch |err| {
        std.debug.print("bquery: {s}\n", .{@errorName(err)});
        std.process.exit(1);
    };
}
fn write(io: std.Io, value: []const u8) !void {
    try std.Io.File.stdout().writeStreamingAll(io, value);
}
fn execute(a: A, io: std.Io, video: *q.video.Runtime, sql: []const u8, files: []const []const u8, options: Options, terminal: bool, input: *std.Io.Reader) !void {
    var check = q.control.RequestControl{ .io = io };
    const c = q.types.Control{ .io = io, .check_context = &check, .check_fn = q.control.RequestControl.canceled };
    const parsed = try q.engine.parse(a, sql);
    const paths = if (parsed.sourceFile()) |filename| blk: {
        if (files.len != 0) return error.ConflictingInputFiles;
        break :blk try a.dupe([]const u8, &.{filename});
    } else files;
    if (paths.len == 0 and !parsed.show_indexes) return error.InputFileRequired;
    if (!parsed.uses(&.{ "person_count", "object_count", "sam3", "sam3_count", "sam3_score" })) {
        check.deadline_ns = std.Io.Timestamp.now(io, .awake).nanoseconds + 120 * std.time.ns_per_s;
    }
    var tables = q.types.Tables.init(a);
    var parts: std.ArrayList(q.types.Table) = .empty;
    for (if (parsed.show_indexes) &.{} else paths) |path| {
        const all = try q.mp4.read(a, c, path, path, parsed.source());
        try parts.append(a, all.get(parsed.source()) orelse return error.UnknownTable);
    }
    if (!parsed.show_indexes) try tables.put(parsed.source(), try q.types.combine(a, parts.items));
    const ProgressOutput = struct {
        io: std.Io,
        terminal: bool,
        fn send(_: *anyopaque, _: q.types.Result, _: bool) !void {}
        fn progress(ctx: *anyopaque, value: q.engine.Progress) !void {
            const self: *@This() = @ptrCast(@alignCast(ctx));
            var buffer: [1024]u8 = undefined;
            const percent = if (value.total == 0) @as(usize, 100) else value.completed * 100 / value.total;
            const label = switch (value.phase) {
                .preparing => "Preparing",
                .waiting => "Waiting for",
                .indexing => "Indexing",
                .saving => "Saving",
                .complete => "Built",
            };
            const stderr = std.Io.File.stderr();
            if (self.terminal) try stderr.writeStreamingAll(self.io, "\r\x1b[2K");
            try stderr.writeStreamingAll(self.io, label);
            try stderr.writeStreamingAll(self.io, " ");
            try stderr.writeStreamingAll(self.io, value.index);
            const line = try std.fmt.bufPrint(&buffer, ": {d}/{d} frames ({d}%) · {d} remaining{s}", .{ value.completed, value.total, percent, value.total - value.completed, if (!self.terminal or value.phase == .complete) "\n" else "" });
            try stderr.writeStreamingAll(self.io, line);
        }
    };
    var progress = ProgressOutput{ .io = io, .terminal = try std.Io.File.stderr().isTty(io) };
    const result = q.engine.execute(a, c, parsed, tables, try video.functions(a), if (parsed.create_index != null) .{ .context = &progress, .send = ProgressOutput.send, .progress = ProgressOutput.progress } else null) catch |err| {
        if (parsed.create_index != null and progress.terminal) std.Io.File.stderr().writeStreamingAll(io, "\n") catch {};
        return err;
    };
    if (options.json) {
        try write(io, try std.json.Stringify.valueAlloc(a, result, .{ .whitespace = .indent_2 }));
        try write(io, "\n");
        return;
    }
    const table = try q.cli.Table.init(a, result);
    const paged = terminal and options.pager;
    var start: usize = 0;
    while (true) {
        const end = if (paged) start + @min(options.page_size, result.rows.len - start) else result.rows.len;
        try write(io, try table.page(a, start, end));
        if (end == result.rows.len) break;
        try write(io, try std.fmt.allocPrint(a, "-- Rows {d}-{d} of {d} · Enter: next page, q: stop -- ", .{ start + 1, end, result.rows.len }));
        const line = (try input.takeDelimiter('\n')) orelse break;
        if (std.mem.eql(u8, std.mem.trim(u8, line, " \t\r"), "q")) break;
        start = end;
    }
    try write(io, try std.fmt.allocPrint(a, "({d} {s})\n", .{ result.rows.len, if (result.rows.len == 1) "row" else "rows" }));
}
fn executeRepl(a: A, io: std.Io, video: *q.video.Runtime, sql: []const u8, files: []const []const u8, options: Options, terminal: bool, input: *std.Io.Reader) !bool {
    if (std.mem.trim(u8, sql, " \t\r\n").len == 0) return true;
    var arena = std.heap.ArenaAllocator.init(a);
    defer arena.deinit();
    execute(arena.allocator(), io, video, sql, files, options, terminal, input) catch |err| {
        std.debug.print("ERROR: {s}\n", .{@errorName(err)});
        return false;
    };
    return true;
}
fn repl(a: A, io: std.Io, video: *q.video.Runtime, files: []const []const u8, initial: Options, terminal: bool, input: *std.Io.Reader) !void {
    var options = initial;
    var pending: std.ArrayList(u8) = .empty;
    defer pending.deinit(a);
    var failed = false;
    if (terminal) try write(io, "Binary Query SQL console. Use ; to run a query, \\h for help, \\q to quit.\n");
    while (true) {
        if (terminal) try write(io, if (std.mem.trim(u8, pending.items, " \t\r\n").len == 0) "bquery=> " else "bquery-> ");
        const raw = (try input.takeDelimiter('\n')) orelse {
            if (!try executeRepl(a, io, video, pending.items, files, options, terminal, input)) failed = true;
            break;
        };
        const line = std.mem.trim(u8, raw, " \t\r");
        if (std.mem.startsWith(u8, line, "\\")) {
            if (std.mem.eql(u8, line, "\\q")) break;
            if (std.mem.eql(u8, line, "\\h") or std.mem.eql(u8, line, "\\help") or std.mem.eql(u8, line, "\\?")) {
                try write(io, help);
                continue;
            }
            if (std.mem.eql(u8, line, "\\r")) {
                pending.clearRetainingCapacity();
                continue;
            }
            if (std.mem.eql(u8, line, "\\p")) {
                try write(io, pending.items);
                continue;
            }
            if (std.mem.eql(u8, line, "\\g")) {
                if (!try executeRepl(a, io, video, pending.items, files, options, terminal, input)) failed = true;
                pending.clearRetainingCapacity();
                continue;
            }
            if (std.mem.eql(u8, line, "\\pset pager off")) {
                options.pager = false;
                continue;
            }
            if (std.mem.eql(u8, line, "\\pset pager on")) {
                options.pager = true;
                continue;
            }
            if (std.mem.eql(u8, line, "\\pset format json")) {
                options.json = true;
                continue;
            }
            if (std.mem.eql(u8, line, "\\pset format table")) {
                options.json = false;
                continue;
            }
            std.debug.print("Unknown command. Use \\h for help.\n", .{});
            failed = true;
            continue;
        }
        try pending.appendSlice(a, raw);
        try pending.append(a, '\n');
        while (q.cli.statementEnd(pending.items)) |end| {
            if (!try executeRepl(a, io, video, pending.items[0..end], files, options, terminal, input)) failed = true;
            const remaining = pending.items.len - end - 1;
            std.mem.copyForwards(u8, pending.items[0..remaining], pending.items[end + 1 ..]);
            pending.shrinkRetainingCapacity(remaining);
        }
    }
    if (failed and !terminal) return error.QueryFailed;
}
fn run(init: std.process.Init) !void {
    q.control.install();
    var arena = std.heap.ArenaAllocator.init(init.gpa);
    defer arena.deinit();
    const a = arena.allocator();
    const args = try init.minimal.args.toSlice(a);
    var options: Options = .{};
    var sql: ?[]const u8 = null;
    var i: usize = 1;
    while (i < args.len and std.mem.startsWith(u8, args[i], "-")) : (i += 1) {
        if (std.mem.eql(u8, args[i], "--")) {
            i += 1;
            break;
        }
        if (std.mem.eql(u8, args[i], "-h") or std.mem.eql(u8, args[i], "--help")) {
            try write(init.io, help);
            return;
        }
        const flag = if (std.mem.startsWith(u8, args[i], "--")) args[i][1..] else args[i];
        if (std.mem.eql(u8, flag, "-json")) {
            options.json = true;
            continue;
        }
        if (std.mem.eql(u8, flag, "-no-pager")) {
            options.pager = false;
            continue;
        }
        if (std.mem.startsWith(u8, flag, "-query=")) {
            sql = flag[7..];
            continue;
        }
        if (i + 1 >= args.len) return error.InvalidArgument;
        if (std.mem.eql(u8, flag, "-query") or std.mem.eql(u8, flag, "-c")) {
            i += 1;
            sql = args[i];
            continue;
        }
        if (std.mem.eql(u8, flag, "-page-size")) {
            i += 1;
            options.page_size = std.fmt.parseInt(usize, args[i], 10) catch return error.InvalidArgument;
            if (options.page_size == 0) return error.InvalidArgument;
            continue;
        }
        return error.InvalidArgument;
    }
    const terminal = try std.Io.File.stdin().isTty(init.io) and try std.Io.File.stdout().isTty(init.io);
    var buffer: [65536]u8 = undefined;
    var reader = std.Io.File.stdin().readerStreaming(init.io, &buffer);
    var video = try q.video.Runtime.init(init.gpa, init.io);
    defer video.deinit();
    if (sql) |query| try execute(a, init.io, &video, std.mem.trimEnd(u8, query, " \t\r\n;"), args[i..], options, terminal, &reader.interface) else try repl(init.gpa, init.io, &video, args[i..], options, terminal, &reader.interface);
}
