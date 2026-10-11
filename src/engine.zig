const std = @import("std");
const t = @import("types.zig");
const p = @import("parse.zig");
pub const parse = p.parse;
pub const Query = p.Query;
const A = std.mem.Allocator;
const V = t.Value;
pub const Function = struct {
    arity: usize,
    context: *anyopaque,
    evaluate: *const fn (*anyopaque, A, t.Control, []const V) anyerror!V,
    prepare: ?*const fn (*anyopaque, A, t.Control, []const []const V) anyerror!void = null,
};
pub const Functions = std.StringHashMap(Function);
pub const Emit = struct { context: *anyopaque, send: *const fn (*anyopaque, t.Result, bool) anyerror!void };
fn eq(a: []const u8, b: []const u8) bool {
    return std.mem.eql(u8, a, b);
}
pub fn compare(a: V, b: V) ?std.math.Order {
    if (a.number()) |x| {
        const y = b.number() orelse return null;
        if (!std.math.isFinite(x) or !std.math.isFinite(y)) return null;
        return std.math.order(x, y);
    }
    return switch (a) {
        .string => |x| if (b == .string) std.mem.order(u8, x, b.string) else null,
        .boolean => |x| if (b == .boolean) std.math.order(@intFromBool(x), @intFromBool(b.boolean)) else null,
        else => null,
    };
}
fn match(op: @FieldType(p.Predicate, "op"), a: V, b: V, upper: V) bool {
    if (op == .is_null) return a == .null;
    if (op == .is_not_null) return a != .null;
    if (op == .between or op == .not_between) {
        const lo = compare(a, b);
        const hi = compare(a, upper);
        if (op == .between) return lo != null and hi != null and lo.? != .lt and hi.? != .gt;
        return (lo != null and lo.? == .lt) or (hi != null and hi.? == .gt);
    }
    const c = compare(a, b) orelse return false;
    return switch (op) {
        .eq => c == .eq,
        .ne => c != .eq,
        .lt => c == .lt,
        .le => c != .gt,
        .gt => c == .gt,
        .ge => c != .lt,
        else => false,
    };
}
fn validate(e: *p.Expr, table: t.Table, functions: Functions, allow_window: bool) anyerror!void {
    switch (e.kind) {
        .column => if (!table.has(e.name)) return error.UnknownColumn,
        .literal => {},
        .call => {
            const f = functions.get(e.name) orelse return error.UnknownFunction;
            if (e.args.len != f.arity) return error.InvalidFunctionArity;
        },
        .lag => {
            if (!allow_window) return error.WindowNotAllowedHere;
            for (e.window.partition) |c| {
                if (!table.has(c)) return error.UnknownPartitionColumn;
            }
            for (e.window.order) |o| {
                if (!table.has(o.column)) return error.UnknownOrderColumn;
            }
        },
    }
    for (e.args) |arg| try validate(arg, table, functions, allow_window and e.kind != .lag);
}
const Filtered = struct {
    table: t.Table,
    indices: []const usize,
    fn get(ctx: *const anyopaque, row: usize, col: usize) V {
        const f: *const @This() = @ptrCast(@alignCast(ctx));
        return f.table.get(f.table.context, f.indices[row], col);
    }
    fn asTable(f: *Filtered) t.Table {
        return .{ .columns = f.table.columns, .count = f.indices.len, .context = f, .get = get };
    }
};
const SortContext = struct {
    values: []const []const V,
    orders: []const p.Order,
    fn less(s: @This(), a: usize, b: usize) bool {
        for (s.orders, s.values) |o, values| {
            const x = values[a];
            const y = values[b];
            if (x == .null and y == .null) continue;
            if (x == .null) return o.nulls_first;
            if (y == .null) return !o.nulls_first;
            const c = compare(x, y) orelse continue;
            if (c != .eq) return if (o.desc) c == .gt else c == .lt;
        }
        return a < b;
    }
};
fn sorted(a: A, control: t.Control, active: []usize, values: []const []const V, orders: []const p.Order) !void {
    _ = a;
    for (values) |column| {
        var exemplar: ?V = null;
        for (column) |v| {
            if (v == .null) continue;
            if (exemplar) |x| {
                if (compare(x, v) == null) return error.IncomparableOrderValues;
            } else {
                if (compare(v, v) == null) return error.NonScalarOrderValue;
                exemplar = v;
            }
        }
    }
    try control.check();
    std.mem.sort(usize, active, SortContext{ .values = values, .orders = orders }, SortContext.less);
    try control.check();
}
const CacheKey = struct { expression: *p.Expr, row: usize };
const Layout = struct { order: []usize, rank: []usize, start: []usize };
const Eval = struct {
    a: A,
    control: t.Control,
    table: t.Table,
    functions: Functions,
    cache: std.AutoHashMap(CacheKey, V),
    windows: std.AutoHashMap(*p.Expr, Layout),
    fn init(a: A, control: t.Control, table: t.Table, functions: Functions) Eval {
        return .{ .a = a, .control = control, .table = table, .functions = functions, .cache = .init(a), .windows = .init(a) };
    }
    fn value(e: *Eval, expr: *p.Expr, row: usize) anyerror!V {
        try e.control.check();
        if (expr.kind == .column) return e.table.value(row, expr.name);
        if (expr.kind == .literal) return expr.value;
        const key: CacheKey = .{ .expression = expr, .row = row };
        if (e.cache.get(key)) |v| return v;
        const result: V = if (expr.kind == .lag) blk: {
            const l = try e.layout(expr);
            const offset_value: V = if (expr.args.len > 1) try e.value(expr.args[1], row) else .{ .int = 1 };
            if (offset_value == .null) break :blk .null;
            const n = offset_value.number() orelse return error.InvalidLagOffset;
            if (n < 0 or n != @trunc(n) or n > std.math.maxInt(i64)) return error.InvalidLagOffset;
            const offset: usize = @intFromFloat(n);
            const rank = l.rank[row];
            if (offset <= rank - l.start[row]) break :blk try e.value(expr.args[0], l.order[rank - offset]);
            break :blk if (expr.args.len == 3) try e.value(expr.args[2], row) else .null;
        } else blk: {
            var args: [16]V = undefined;
            for (expr.args, 0..) |arg, i| args[i] = try e.value(arg, row);
            const f = e.functions.get(expr.name).?;
            break :blk try f.evaluate(f.context, e.a, e.control, args[0..expr.args.len]);
        };
        try e.cache.put(key, result);
        return result;
    }
    fn layout(e: *Eval, expr: *p.Expr) !Layout {
        if (e.windows.get(expr)) |l| return l;
        const n = e.table.count;
        const order = try indices(e.a, n);
        var orders: std.ArrayList(p.Order) = .empty;
        var values: std.ArrayList([]const V) = .empty;
        // Partition keys are compared by type first: mixed scalar types are
        // separate groups even when their text representation is identical.
        const partitions = try e.a.alloc([]const u8, n);
        for (0..n) |i| {
            var writer: std.Io.Writer.Allocating = .init(e.a);
            for (expr.window.partition) |col| {
                const v = e.table.value(i, col);
                if (v.number()) |num| try writer.writer.print("number:{d};", .{num}) else switch (v) {
                    .null => try writer.writer.writeAll("null;"),
                    .string => |s| try writer.writer.print("string:{d}:{s};", .{ s.len, s }),
                    .boolean => |b| try writer.writer.print("bool:{};", .{b}),
                    else => return error.NonScalarPartitionValue,
                }
            }
            partitions[i] = try writer.toOwnedSlice();
        }
        const keys = try e.a.alloc(V, n);
        for (partitions, 0..) |s, i| keys[i] = .{ .string = s };
        try values.append(e.a, keys);
        try orders.append(e.a, .{ .column = "partition" });
        for (expr.window.order) |o| {
            try orders.append(e.a, o);
            const column = try e.a.alloc(V, n);
            for (column, 0..) |*v, i| v.* = e.table.value(i, o.column);
            try values.append(e.a, column);
        }
        try sorted(e.a, e.control, order, values.items, orders.items);
        const rank = try e.a.alloc(usize, n);
        const starts = try e.a.alloc(usize, n);
        var start: usize = 0;
        for (order, 0..) |row, i| {
            if (i > 0 and !eq(partitions[row], partitions[order[i - 1]])) start = i;
            rank[row] = i;
            starts[row] = start;
        }
        const l = Layout{ .order = order, .rank = rank, .start = starts };
        try e.windows.put(expr, l);
        return l;
    }
    fn matches(e: *Eval, row: usize, predicates: []const p.Predicate) !bool {
        for (predicates) |pred| {
            const left = try e.value(pred.left, row);
            const right = if (pred.right) |r| try e.value(r, row) else .null;
            const upper = if (pred.upper) |r| try e.value(r, row) else .null;
            if (!match(pred.op, left, right, upper)) return false;
        }
        return true;
    }
    fn prepare(e: *Eval, expr: *p.Expr, rows: []const usize) anyerror!void {
        if (expr.kind != .call) return;
        for (expr.args) |arg| try e.prepare(arg, rows);
        const f = e.functions.get(expr.name).?;
        const prefetch = f.prepare orelse return;
        var arena = std.heap.ArenaAllocator.init(e.a);
        defer arena.deinit();
        const a = arena.allocator();
        var arguments: std.ArrayList([]const V) = .empty;
        for (rows) |row| {
            if (e.cache.contains(.{ .expression = expr, .row = row })) continue;
            const args = try a.alloc(V, expr.args.len);
            for (expr.args, 0..) |arg, i| args[i] = try e.value(arg, row);
            try arguments.append(a, args);
        }
        if (arguments.items.len > 0) try prefetch(f.context, a, e.control, arguments.items);
    }
};
fn indices(a: A, n: usize) ![]usize {
    const result = try a.alloc(usize, n);
    for (result, 0..) |*r, i| r.* = i;
    return result;
}
fn computed(pred: p.Predicate) bool {
    return pred.left.kind == .call or (if (pred.right) |r| r.kind == .call else false) or (if (pred.upper) |r| r.kind == .call else false);
}
fn alias(selects: []const p.Selection, name: []const u8) ?*p.Expr {
    for (selects) |s| {
        if (eq(s.name, name)) return s.expression;
    }
    return null;
}
fn orderRows(e: *Eval, selects: []const p.Selection, orders: []const p.Order) ![]usize {
    const active = try indices(e.a, e.table.count);
    const values = try e.a.alloc([]const V, orders.len);
    for (orders, 0..) |o, j| {
        const column = try e.a.alloc(V, e.table.count);
        for (column, 0..) |*v, i| v.* = if (alias(selects, o.column)) |expr| try e.value(expr, i) else e.table.value(i, o.column);
        values[j] = column;
    }
    if (orders.len > 0) try sorted(e.a, e.control, active, values, orders);
    return active;
}
pub fn execute(a: A, control: t.Control, q: Query, tables: t.Tables, functions: Functions, emit: ?Emit) anyerror!t.Result {
    try control.check();
    var table: t.Table = undefined;
    if (q.subquery) |inner| {
        const r = try a.create(t.Result);
        r.* = try execute(a, control, inner.*, tables, functions, null);
        table = r.table();
    } else table = tables.get(q.table) orelse return error.UnknownTable;
    var selects = q.selects;
    if (q.star) {
        const list = try a.alloc(p.Selection, table.columns.len);
        for (table.columns, 0..) |col, i| {
            const expr = try a.create(p.Expr);
            expr.* = .{ .kind = .column, .name = col };
            list[i] = .{ .expression = expr, .name = col };
        }
        selects = list;
    }
    const columns = try a.alloc([]const u8, selects.len);
    var has_window = false;
    for (selects, 0..) |s, i| {
        try validate(s.expression, table, functions, true);
        columns[i] = s.name;
        has_window = has_window or s.expression.hasWindow();
    }
    for (q.predicates) |pred| {
        try validate(pred.left, table, functions, false);
        if (pred.right) |r| try validate(r, table, functions, false);
        if (pred.upper) |r| try validate(r, table, functions, false);
    }
    const predicates = try a.alloc(p.Predicate, q.predicates.len);
    var next: usize = 0;
    // Input-only predicates prune frames even when written after inference.
    inline for (.{ false, true }) |expensive| {
        for (q.predicates) |pred| {
            if (computed(pred) == expensive) {
                predicates[next] = pred;
                next += 1;
            }
        }
    }
    var input_order = true;
    for (q.order) |o| {
        const expr = alias(selects, o.column);
        if (expr == null and !table.has(o.column)) return error.UnknownOrderColumn;
        if (expr) |x| {
            if (x.kind != .column) input_order = false;
        }
    }
    var result = t.Result{ .columns = columns, .rows = &.{} };
    if (emit) |out| try out.send(out.context, result, true);
    var e = Eval.init(a, control, table, functions);
    var rows: std.ArrayList([]const V) = .empty;
    var scratch = std.heap.ArenaAllocator.init(a);
    defer scratch.deinit();
    const row_allocator = if (emit != null) scratch.allocator() else a;
    // With input ordering and no windows, LIMIT and streaming may stop as soon
    // as enough matching rows arrive. Cheap predicates precede frame inference.
    if (!has_window and input_order) {
        if (q.limit == 0) return result;
        const active = try orderRows(&e, selects, q.order);
        var matches: usize = 0;
        var produced: usize = 0;
        for (active) |row| {
            if (!try e.matches(row, predicates)) continue;
            matches += 1;
            if (matches <= q.offset) continue;
            if (emit != null) _ = scratch.reset(.retain_capacity);
            const values = try row_allocator.alloc(V, selects.len);
            for (selects, 0..) |s, j| values[j] = try e.value(s.expression, row);
            if (emit) |out| try out.send(out.context, .{ .columns = columns, .rows = &.{values} }, false) else try rows.append(a, values);
            produced += 1;
            if (q.limit) |limit| {
                if (produced >= limit) break;
            }
        }
    } else {
        var selected: std.ArrayList(usize) = .empty;
        for (0..table.count) |row| {
            if (try e.matches(row, predicates)) try selected.append(a, row);
        }
        var filtered = Filtered{ .table = table, .indices = selected.items };
        e = Eval.init(a, control, filtered.asTable(), functions);
        const active = try orderRows(&e, selects, q.order);
        const start = @min(q.offset, active.len);
        const end = if (q.limit) |limit| start + @min(limit, active.len - start) else active.len;
        const page = active[start..end];
        for (page, 0..) |row, index| {
            // Prepare only a bounded batch of output rows after pagination.
            // Window layouts still include the complete filtered input, so
            // LAG can refer to the frame immediately before this page.
            if (index % 32 == 0) {
                for (selects) |s| try e.prepare(s.expression, page[index..@min(index + 32, page.len)]);
            }
            if (emit != null) _ = scratch.reset(.retain_capacity);
            const values = try row_allocator.alloc(V, selects.len);
            for (selects, 0..) |s, j| values[j] = try e.value(s.expression, row);
            if (emit) |out| try out.send(out.context, .{ .columns = columns, .rows = &.{values} }, false) else try rows.append(a, values);
        }
    }
    result.rows = try rows.toOwnedSlice(a);
    return result;
}

test "SQL null range semantics and exact integer comparison" {
    try std.testing.expect(!match(.between, .{ .int = 1 }, .null, .{ .int = 10 }));
    try std.testing.expect(match(.not_between, .{ .int = 12 }, .null, .{ .int = 10 }));
    try std.testing.expectEqual(std.math.Order.gt, compare(.{ .int = 9007199254740993 }, .{ .float = 9007199254740992 }).?);
}
test "window input filtering, defaults, subqueries and pagination" {
    var arena = std.heap.ArenaAllocator.init(std.testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const rows = [_][]const V{ &.{ .{ .int = 3 }, .{ .string = "a" } }, &.{ .{ .int = 1 }, .{ .string = "a" } }, &.{ .{ .int = 2 }, .{ .string = "a" } } };
    const r = t.Result{ .columns = &.{ "n", "part" }, .rows = &rows };
    var tables = t.Tables.init(a);
    try tables.put("frames", r.table());
    const q = try parse(a, "SELECT n, previous FROM (SELECT n, LAG(n, 1, -1) OVER (PARTITION BY part ORDER BY n) AS previous FROM frames) s WHERE n >= 2 ORDER BY n DESC LIMIT 1 OFFSET 1");
    var threaded: std.Io.Threaded = .init(a, .{});
    defer threaded.deinit();
    const result = try execute(a, .{ .io = threaded.io() }, q, tables, Functions.init(a), null);
    try std.testing.expectEqual(@as(usize, 1), result.rows.len);
    try std.testing.expectEqual(@as(i128, 2), result.rows[0][0].int);
    try std.testing.expectEqual(@as(i128, 1), result.rows[0][1].int);
}
test "window function preparation is bounded to the output page and retains preceding rows" {
    var arena = std.heap.ArenaAllocator.init(std.testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const rows = try a.alloc([]const V, 100);
    for (rows, 0..) |*row, i| row.* = try a.dupe(V, &.{.{ .int = @intCast(i + 1) }});
    const input = t.Result{ .columns = &.{"n"}, .rows = rows };
    var tables = t.Tables.init(a);
    try tables.put("frames", input.table());
    const State = struct {
        prepared: usize = 0,
        evaluated: usize = 0,
        batches: usize = 0,
        fn prepare(ctx: *anyopaque, _: A, _: t.Control, arguments: []const []const V) !void {
            const s: *@This() = @ptrCast(@alignCast(ctx));
            try std.testing.expect(arguments.len <= 32);
            for (arguments) |args| {
                try std.testing.expectEqual(@as(i128, @intCast(s.prepared + 11)), args[0].int);
                try std.testing.expectEqual(args[0].int - 1, args[1].int);
                s.prepared += 1;
            }
            s.batches += 1;
        }
        fn evaluate(ctx: *anyopaque, _: A, _: t.Control, args: []const V) !V {
            const s: *@This() = @ptrCast(@alignCast(ctx));
            try std.testing.expect(s.evaluated < s.prepared);
            s.evaluated += 1;
            return .{ .int = args[0].int - args[1].int };
        }
    };
    var state: State = .{};
    var functions = Functions.init(a);
    try functions.put("difference", .{ .arity = 2, .context = &state, .evaluate = State.evaluate, .prepare = State.prepare });
    const query = try parse(a, "SELECT n, difference(n, LAG(n) OVER (ORDER BY n)) AS score FROM frames ORDER BY n LIMIT 35 OFFSET 10");
    const result = try execute(a, .{ .io = std.testing.io }, query, tables, functions, null);
    try std.testing.expectEqual(@as(usize, 35), result.rows.len);
    try std.testing.expectEqual(@as(usize, 35), state.prepared);
    try std.testing.expectEqual(@as(usize, 35), state.evaluated);
    try std.testing.expectEqual(@as(usize, 2), state.batches);
    for (result.rows) |row| try std.testing.expectEqual(@as(i128, 1), row[1].int);
}

test "cheap filters precede inference and LIMIT stops further calls" {
    var arena = std.heap.ArenaAllocator.init(std.testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const rows = [_][]const V{ &.{.{ .int = 1 }}, &.{.{ .int = 2 }}, &.{.{ .int = 3 }}, &.{.{ .int = 4 }} };
    const data = t.Result{ .columns = &.{"n"}, .rows = &rows };
    var tables = t.Tables.init(a);
    try tables.put("frames", data.table());
    const Counter = struct {
        calls: usize = 0,
        fn evaluate(context: *anyopaque, _: A, _: t.Control, args: []const V) !V {
            const counter: *@This() = @ptrCast(@alignCast(context));
            counter.calls += 1;
            if (args[0].int < 3) return error.UnfilteredInference;
            return args[0];
        }
    };
    var counter = Counter{};
    var functions = Functions.init(a);
    try functions.put("inference", .{ .arity = 1, .context = &counter, .evaluate = Counter.evaluate });
    const query = try parse(a, "SELECT inference(n) AS result FROM frames WHERE inference(n) > 0 AND n >= 3 LIMIT 1");
    const result = try execute(a, .{ .io = std.testing.io }, query, tables, functions, null);
    try std.testing.expectEqual(@as(usize, 1), result.rows.len);
    try std.testing.expectEqual(@as(i128, 3), result.rows[0][0].int);
    // WHERE and SELECT use separate expressions; neither touches another row.
    try std.testing.expectEqual(@as(usize, 2), counter.calls);
}

test "streamed query rows serialize structured mask values" {
    var arena = std.heap.ArenaAllocator.init(std.testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const data = t.Result{ .columns = &.{ "frame_index", "segmentation" }, .rows = &.{&.{ .{ .int = 3000 }, .{ .json = "{\"count\":1,\"masks\":[{\"rle\":[0,2]}]}" } }} };
    var tables = t.Tables.init(a);
    try tables.put("frames", data.table());
    const Stream = struct {
        a: A,
        rows: usize = 0,
        fn send(context: *anyopaque, result: t.Result, schema: bool) !void {
            const stream: *@This() = @ptrCast(@alignCast(context));
            const encoded = try std.json.Stringify.valueAlloc(stream.a, result, .{});
            const decoded = try std.json.parseFromSlice(std.json.Value, stream.a, encoded, .{});
            if (!schema) {
                const mask = decoded.value.object.get("rows").?.array.items[0].object.get("segmentation").?;
                try std.testing.expectEqual(@as(i64, 1), mask.object.get("count").?.integer);
                stream.rows += result.rows.len;
            }
        }
    };
    var stream = Stream{ .a = a };
    _ = try execute(a, .{ .io = std.testing.io }, try parse(a, "SELECT frame_index, segmentation FROM frames WHERE frame_index = 3000 LIMIT 1"), tables, Functions.init(a), .{ .context = &stream, .send = Stream.send });
    try std.testing.expectEqual(@as(usize, 1), stream.rows);
}
