const std = @import("std");
const t = @import("types.zig");
const A = std.mem.Allocator;
pub const Order = struct { column: []const u8, desc: bool = false, nulls_first: bool = false };
pub const Window = struct { partition: []const []const u8 = &.{}, order: []const Order = &.{} };
pub const Expr = struct {
    kind: enum { column, literal, call, lag },
    name: []const u8 = "",
    value: t.Value = .null,
    args: []const *Expr = &.{},
    window: Window = .{},
    pub fn label(e: *const Expr, a: A) anyerror![]const u8 {
        if (e.kind == .column) return e.name;
        if (e.kind == .literal) return switch (e.value) {
            .string => |s| s,
            .null => "null",
            .boolean => |b| if (b) "true" else "false",
            .int => |n| try std.fmt.allocPrint(a, "{d}", .{n}),
            .float => |n| try std.fmt.allocPrint(a, "{d}", .{n}),
            else => error.InvalidLiteral,
        };
        var out: std.Io.Writer.Allocating = .init(a);
        try out.writer.print("{s}(", .{e.name});
        for (e.args, 0..) |arg, i| {
            if (i > 0) try out.writer.writeAll(", ");
            try out.writer.writeAll(try arg.label(a));
        }
        try out.writer.writeByte(')');
        if (e.kind == .lag) {
            try out.writer.writeAll(" OVER (");
            if (e.window.partition.len > 0) {
                try out.writer.writeAll("PARTITION BY ");
                for (e.window.partition, 0..) |col, i| {
                    if (i > 0) try out.writer.writeAll(", ");
                    try out.writer.writeAll(col);
                }
                if (e.window.order.len > 0) try out.writer.writeByte(' ');
            }
            if (e.window.order.len > 0) {
                try out.writer.writeAll("ORDER BY ");
                for (e.window.order, 0..) |order, i| {
                    if (i > 0) try out.writer.writeAll(", ");
                    try out.writer.writeAll(order.column);
                    if (order.desc) try out.writer.writeAll(" DESC");
                    if (order.nulls_first != order.desc) try out.writer.writeAll(if (order.nulls_first) " NULLS FIRST" else " NULLS LAST");
                }
            }
            try out.writer.writeByte(')');
        }
        return out.toOwnedSlice();
    }
    pub fn hasWindow(e: *const Expr) bool {
        if (e.kind == .lag) return true;
        for (e.args) |arg| {
            if (arg.hasWindow()) return true;
        }
        return false;
    }
    pub fn uses(e: *const Expr, names: []const []const u8) bool {
        if (e.kind == .call) for (names) |name| {
            if (std.ascii.eqlIgnoreCase(name, e.name)) return true;
        };
        for (e.args) |arg| {
            if (arg.uses(names)) return true;
        }
        return false;
    }
};
pub const Selection = struct { expression: *Expr, name: []const u8 };
pub const Predicate = struct { left: *Expr, right: ?*Expr = null, upper: ?*Expr = null, op: enum { eq, ne, lt, le, gt, ge, is_null, is_not_null, between, not_between } };
pub const Query = struct {
    show_indexes: bool = false,
    create_index: ?[]const u8 = null,
    table: []const u8 = "",
    filename: ?[]const u8 = null,
    subquery: ?*Query = null,
    selects: []const Selection = &.{},
    star: bool = false,
    predicates: []const Predicate = &.{},
    order: []const Order = &.{},
    limit: ?usize = null,
    offset: usize = 0,
    pub fn source(q: Query) []const u8 {
        return if (q.subquery) |s| s.source() else q.table;
    }
    pub fn sourceFile(q: Query) ?[]const u8 {
        return if (q.subquery) |s| s.sourceFile() else q.filename;
    }
    pub fn hasWindow(q: Query) bool {
        for (q.selects) |s| {
            if (s.expression.hasWindow()) return true;
        }
        return false;
    }
    pub fn uses(q: Query, names: []const []const u8) bool {
        for (q.selects) |s| {
            if (s.expression.uses(names)) return true;
        }
        for (q.predicates) |pred| {
            if (pred.left.uses(names)) return true;
            if (pred.right) |r| {
                if (r.uses(names)) return true;
            }
            if (pred.upper) |r| {
                if (r.uses(names)) return true;
            }
        }
        return if (q.subquery) |inner| inner.uses(names) else false;
    }
};
const Token = struct { text: []const u8, quoted: bool = false };
pub fn parse(a: A, sql: []const u8) !Query {
    if (sql.len > 65536) return error.QueryTooLarge;
    var tokens: std.ArrayList(Token) = .empty;
    var i: usize = 0;
    while (i < sql.len) {
        if (std.ascii.isWhitespace(sql[i])) {
            i += 1;
            continue;
        }
        if (sql[i] == '\'') {
            i += 1;
            var string: std.ArrayList(u8) = .empty;
            var closed = false;
            while (i < sql.len) {
                if (sql[i] == '\'') {
                    i += 1;
                    if (i < sql.len and sql[i] == '\'') {
                        try string.append(a, '\'');
                        i += 1;
                        continue;
                    }
                    closed = true;
                    break;
                }
                try string.append(a, sql[i]);
                i += 1;
            }
            if (!closed) return error.UnterminatedString;
            try tokens.append(a, .{ .text = try string.toOwnedSlice(a), .quoted = true });
            continue;
        }
        const start = i;
        if (std.mem.indexOfScalar(u8, "(),*;=<>!", sql[i]) != null) {
            i += 1;
            if (i < sql.len and (sql[i] == '=' or (sql[start] == '<' and sql[i] == '>')) and std.mem.indexOfScalar(u8, "<>!", sql[start]) != null) i += 1;
        } else while (i < sql.len and std.mem.indexOfScalar(u8, " \t\r\n(),*;=<>!'", sql[i]) == null) : (i += 1) {}
        try tokens.append(a, .{ .text = sql[start..i] });
    }
    var p = Parser{ .a = a, .tokens = tokens.items };
    const q = try p.query();
    _ = p.accept(";");
    if (p.i != p.tokens.len) return error.UnexpectedToken;
    return q;
}
const Parser = struct {
    a: A,
    tokens: []const Token,
    i: usize = 0,
    depth: usize = 0,
    fn peek(p: *Parser, text: []const u8) bool {
        return p.i < p.tokens.len and !p.tokens[p.i].quoted and std.ascii.eqlIgnoreCase(p.tokens[p.i].text, text);
    }
    fn accept(p: *Parser, text: []const u8) bool {
        if (!p.peek(text)) return false;
        p.i += 1;
        return true;
    }
    fn take(p: *Parser, text: []const u8) !void {
        if (!p.accept(text)) return error.ExpectedToken;
    }
    fn identifier(p: *Parser) ![]const u8 {
        if (p.i >= p.tokens.len or p.tokens[p.i].quoted) return error.ExpectedIdentifier;
        const text = p.tokens[p.i].text;
        if (text.len == 0) return error.ExpectedIdentifier;
        for (text, 0..) |c, n| {
            if (!(std.ascii.isAlphabetic(c) or c == '_' or (n > 0 and std.ascii.isDigit(c)) or c >= 128)) return error.ExpectedIdentifier;
        }
        p.i += 1;
        return std.ascii.allocLowerString(p.a, text);
    }
    fn query(p: *Parser) anyerror!Query {
        p.depth += 1;
        defer p.depth -= 1;
        if (p.depth > 32) return error.QueryNestingLimit;
        var q: Query = .{};
        if (p.accept("show")) {
            if (p.depth != 1) return error.UnsupportedStatement;
            try p.take("indexes");
            q.show_indexes = true;
            return q;
        }
        if (p.accept("create")) {
            if (p.depth != 1) return error.UnsupportedIndex;
            try p.take("index");
            q.create_index = try p.identifier();
            try p.take("on");
            q.table = try p.identifier();
            if (!std.mem.eql(u8, q.table, "frames")) return error.UnsupportedIndex;
            try p.take("(");
            if (p.i >= p.tokens.len or !p.tokens[p.i].quoted or p.tokens[p.i].text.len == 0 or std.mem.indexOfScalar(u8, p.tokens[p.i].text, 0) != null) return error.InvalidTableSource;
            q.filename = p.tokens[p.i].text;
            p.i += 1;
            try p.take(")");
            try p.take("(");
            const expr = try p.expression(0);
            if (expr.kind != .call or !std.mem.eql(u8, expr.name, "person_count") or expr.args.len != 1 or expr.args[0].kind != .column or !std.mem.eql(u8, expr.args[0].name, "frame")) return error.UnsupportedIndexExpression;
            try p.take(")");
            const frame = try p.a.create(Expr);
            frame.* = .{ .kind = .column, .name = "frame" };
            q.selects = try p.a.dupe(Selection, &.{ .{ .expression = frame, .name = "frame" }, .{ .expression = expr, .name = "people" } });
        } else {
            try p.take("select");
            if (p.accept("*")) q.star = true else {
                var selects: std.ArrayList(Selection) = .empty;
                while (true) {
                    const e = try p.expression(0);
                    const name = if (p.accept("as")) try p.identifier() else try e.label(p.a);
                    for (selects.items) |s| {
                        if (std.mem.eql(u8, s.name, name)) return error.DuplicateSelectedColumn;
                    }
                    try selects.append(p.a, .{ .expression = e, .name = name });
                    if (!p.accept(",")) break;
                }
                q.selects = try selects.toOwnedSlice(p.a);
            }
            try p.take("from");
            if (p.accept("(")) {
                const inner = try p.a.create(Query);
                inner.* = try p.query();
                q.subquery = inner;
                try p.take(")");
                _ = p.accept("as");
                _ = try p.identifier();
            } else {
                q.table = try p.identifier();
                if (p.accept("(")) {
                    if (p.i >= p.tokens.len or !p.tokens[p.i].quoted or p.tokens[p.i].text.len == 0 or std.mem.indexOfScalar(u8, p.tokens[p.i].text, 0) != null) return error.InvalidTableSource;
                    q.filename = p.tokens[p.i].text;
                    p.i += 1;
                    if (!p.accept(")")) return error.InvalidTableSource;
                }
            }
        }
        if (p.accept("where")) {
            var predicates: std.ArrayList(Predicate) = .empty;
            while (true) {
                const left = try p.expression(0);
                var pred = Predicate{ .left = left, .op = .eq };
                if (p.accept("is")) {
                    pred.op = if (p.accept("not")) .is_not_null else .is_null;
                    try p.take("null");
                } else if (p.peek("between") or p.peek("not")) {
                    pred.op = if (p.accept("not")) .not_between else .between;
                    try p.take("between");
                    pred.right = try p.expression(0);
                    try p.take("and");
                    pred.upper = try p.expression(0);
                } else {
                    if (p.i >= p.tokens.len or p.tokens[p.i].quoted) return error.ExpectedOperator;
                    const op = p.tokens[p.i].text;
                    p.i += 1;
                    pred.op = if (std.mem.eql(u8, op, "=")) .eq else if (std.mem.eql(u8, op, "!=") or std.mem.eql(u8, op, "<>")) .ne else if (std.mem.eql(u8, op, "<")) .lt else if (std.mem.eql(u8, op, "<=")) .le else if (std.mem.eql(u8, op, ">")) .gt else if (std.mem.eql(u8, op, ">=")) .ge else return error.UnsupportedOperator;
                    pred.right = try p.expression(0);
                }
                try predicates.append(p.a, pred);
                if (!p.accept("and")) break;
            }
            q.predicates = try predicates.toOwnedSlice(p.a);
        }
        if (p.accept("order")) {
            try p.take("by");
            q.order = try p.orders();
        }
        var limit_seen = false;
        var offset_seen = false;
        while (p.peek("limit") or p.peek("offset")) {
            const limit = p.accept("limit");
            if (!limit) try p.take("offset");
            if ((limit and limit_seen) or (!limit and offset_seen)) return error.DuplicatePagination;
            if (p.i >= p.tokens.len or p.tokens[p.i].quoted) return error.InvalidPagination;
            const n = std.fmt.parseInt(usize, p.tokens[p.i].text, 10) catch return error.InvalidPagination;
            if (n > std.math.maxInt(isize)) return error.InvalidPagination;
            p.i += 1;
            if (limit) {
                q.limit = n;
                limit_seen = true;
            } else {
                q.offset = n;
                offset_seen = true;
            }
        }
        if (q.create_index != null) {
            if (q.order.len != 0 or q.limit != null or q.offset != 0) return error.UnsupportedIndex;
            for (q.predicates) |pred| {
                if (pred.left.kind != .column or (if (pred.right) |r| r.kind != .literal else false) or (if (pred.upper) |r| r.kind != .literal else false)) return error.UnsupportedIndexPredicate;
            }
        }
        return q;
    }
    fn expression(p: *Parser, depth: usize) anyerror!*Expr {
        if (depth > 32) return error.ExpressionNestingLimit;
        if (p.i >= p.tokens.len) return error.ExpectedExpression;
        const token = p.tokens[p.i];
        const e = try p.a.create(Expr);
        e.* = .{ .kind = .literal };
        if (token.quoted) {
            p.i += 1;
            e.value = .{ .string = token.text };
            return e;
        }
        if (p.accept("null")) return e;
        if (p.accept("true")) {
            e.value = .{ .boolean = true };
            return e;
        }
        if (p.accept("false")) {
            e.value = .{ .boolean = false };
            return e;
        }
        if (token.text.len > 0 and (std.ascii.isDigit(token.text[0]) or std.mem.indexOfScalar(u8, "+-.", token.text[0]) != null)) {
            e.value = try number(token.text);
            p.i += 1;
            return e;
        }
        e.name = try p.identifier();
        e.kind = .column;
        if (!p.accept("(")) return e;
        e.kind = .call;
        var args: std.ArrayList(*Expr) = .empty;
        if (!p.accept(")")) {
            while (true) {
                if (args.items.len >= 16) return error.TooManyArguments;
                try args.append(p.a, try p.expression(depth + 1));
                if (!p.accept(",")) break;
            }
            try p.take(")");
        }
        e.args = try args.toOwnedSlice(p.a);
        if (std.mem.eql(u8, e.name, "lag")) {
            if (e.args.len < 1 or e.args.len > 3) return error.InvalidLagArity;
            e.kind = .lag;
            try p.take("over");
            try p.take("(");
            if (p.accept("partition")) {
                try p.take("by");
                var columns: std.ArrayList([]const u8) = .empty;
                while (true) {
                    try columns.append(p.a, try p.identifier());
                    if (!p.accept(",")) break;
                }
                e.window.partition = try columns.toOwnedSlice(p.a);
            }
            if (p.accept("order")) {
                try p.take("by");
                e.window.order = try p.orders();
            }
            try p.take(")");
        }
        return e;
    }
    fn orders(p: *Parser) ![]const Order {
        var list: std.ArrayList(Order) = .empty;
        while (true) {
            var o = Order{ .column = try p.identifier() };
            o.desc = p.accept("desc");
            o.nulls_first = o.desc;
            if (!o.desc) _ = p.accept("asc");
            if (p.accept("nulls")) {
                if (p.accept("first")) o.nulls_first = true else if (p.accept("last")) o.nulls_first = false else return error.InvalidNullOrdering;
            }
            try list.append(p.a, o);
            if (!p.accept(",")) break;
        }
        return list.toOwnedSlice(p.a);
    }
};
pub fn number(s: []const u8) !t.Value {
    if (s.len > 128) return error.NumericLiteralTooLong;
    if (std.mem.indexOfAny(u8, s, "eE")) |i| {
        const exp = std.fmt.parseInt(i32, s[i + 1 ..], 10) catch return error.InvalidNumber;
        if (exp < -400 or exp > 400) return error.InvalidExponent;
    }
    const f = std.fmt.parseFloat(f64, s) catch return error.InvalidNumber;
    if (!std.math.isFinite(f)) return error.InvalidNumber;
    const precise = std.fmt.parseFloat(f128, s) catch return error.InvalidNumber;
    if (precise == @trunc(precise) and precise >= std.math.minInt(i64) and precise <= std.math.maxInt(u64)) return .{ .int = @intFromFloat(precise) };
    return .{ .float = f };
}
test "parser windows and derived tables" {
    var arena = std.heap.ArenaAllocator.init(std.testing.allocator);
    defer arena.deinit();
    const q = try parse(arena.allocator(), "SELECT LAG(frame, 0, null) OVER (PARTITION BY path ORDER BY pts_seconds DESC NULLS LAST) AS previous FROM (SELECT * FROM frames) AS f WHERE frame_index NOT BETWEEN 2 AND 5 LIMIT 10 OFFSET 1");
    try std.testing.expectEqualStrings("frames", q.source());
    try std.testing.expectEqual(@as(?usize, 10), q.limit);
    try std.testing.expectEqualStrings("lag(frame, 0, null) OVER (PARTITION BY path ORDER BY pts_seconds DESC NULLS LAST)", try q.selects[0].expression.label(arena.allocator()));
}
test "file-backed tables preserve quoted paths through subqueries" {
    var arena = std.heap.ArenaAllocator.init(std.testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const q = try parse(a, "SELECT * FROM (SELECT frame FROM frames('/tmp/it''s a video.mp4')) AS f LIMIT 1");
    try std.testing.expectEqualStrings("frames", q.source());
    try std.testing.expectEqualStrings("/tmp/it's a video.mp4", q.sourceFile().?);
    try std.testing.expectEqual(@as(?[]const u8, null), (try parse(a, "SELECT * FROM frames")).sourceFile());
    for ([_][]const u8{ "frames()", "frames('')", "frames(42)", "frames(null)", "frames('a', 'b')" }) |source| {
        try std.testing.expectError(error.InvalidTableSource, parse(a, try std.fmt.allocPrint(a, "SELECT * FROM {s}", .{source})));
    }
}

test "CREATE INDEX accepts frame filters and rejects unsupported expressions" {
    var arena = std.heap.ArenaAllocator.init(std.testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    const q = try parse(a, "CREATE INDEX people_idx ON frames('it''s.mp4') (person_count(frame)) WHERE frame_index <= 100;");
    try std.testing.expectEqualStrings("people_idx", q.create_index.?);
    try std.testing.expectEqualStrings("it's.mp4", q.sourceFile().?);
    try std.testing.expect(q.uses(&.{"person_count"}));
    try std.testing.expectEqual(@as(usize, 1), q.predicates.len);
    try std.testing.expectError(error.UnsupportedIndexExpression, parse(a, "CREATE INDEX bad ON frames('video.mp4') (motion(frame, frame))"));
    try std.testing.expectError(error.UnsupportedIndexPredicate, parse(a, "CREATE INDEX bad ON frames('video.mp4') (person_count(frame)) WHERE person_count(frame) > 0"));
    try std.testing.expectError(error.UnsupportedIndex, parse(a, "CREATE INDEX bad ON frames('video.mp4') (person_count(frame)) LIMIT 100"));
}

test "SHOW INDEXES is a standalone statement" {
    var arena = std.heap.ArenaAllocator.init(std.testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    try std.testing.expect((try parse(a, "SHOW INDEXES;")).show_indexes);
    try std.testing.expectError(error.UnexpectedToken, parse(a, "SHOW INDEXES FROM frames"));
    try std.testing.expectError(error.UnsupportedStatement, parse(a, "SELECT * FROM (SHOW INDEXES) AS x"));
}
