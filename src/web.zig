const std = @import("std");
const t = @import("types.zig");
const mp4 = @import("mp4.zig");
const engine = @import("engine.zig");
const video = @import("video.zig");
const control_mod = @import("control.zig");
const A = std.mem.Allocator;
const Request = std.http.Server.Request;
const Source = struct {
    arena: std.heap.ArenaAllocator,
    id: []const u8,
    path: []const u8,
    name: []const u8,
    size: u64,
    mtime: i96,
    tables: t.Tables,
    frames: std.StringHashMap(t.Frame),
    fn unchanged(s: *Source, io: std.Io) !void {
        const st = try std.Io.Dir.cwd().statFile(io, s.path, .{});
        if (st.size != s.size or st.mtime.nanoseconds != s.mtime) return error.SourceChanged;
    }
};
const FrameTable = struct {
    server: *Server,
    source: *Source,
    base: t.Table,
    fn get(ctx: *const anyopaque, row: usize, col: usize) t.Value {
        const wrapper: *const FrameTable = @ptrCast(@alignCast(ctx));
        var v = wrapper.base.get(wrapper.base.context, row, col);
        if (v != .frame) return v;
        const server = wrapper.server;
        const src = wrapper.source;
        server.mutex.lockUncancelable(server.io);
        defer server.mutex.unlock(server.io);
        var buf: [128]u8 = undefined;
        const key = std.fmt.bufPrint(&buf, "{s}-{d}-{d}", .{ src.id, v.frame.track_id, v.frame.frame_index }) catch unreachable;
        if (src.frames.get(key)) |f| return .{ .frame = f };
        const a = src.arena.allocator();
        const id = a.dupe(u8, key) catch return .null;
        v.frame.id = id;
        v.frame.source = src.path;
        src.frames.put(id, v.frame) catch return .null;
        return v;
    }
};
const Server = struct {
    a: A,
    io: std.Io,
    mutex: std.Io.Mutex = .init,
    inference_mutex: std.Io.Mutex = .init,
    sources: std.ArrayList(*Source) = .empty,
    retired: std.ArrayList(*Source) = .empty,
    runtime: video.Runtime,
    fn add(s: *Server, path: []const u8, name: []const u8) !void {
        var arena = std.heap.ArenaAllocator.init(s.a);
        errdefer arena.deinit();
        const a = arena.allocator();
        const abs = try std.Io.Dir.cwd().realPathFileAlloc(s.io, path, a);
        const file = try std.Io.Dir.cwd().openFile(s.io, abs, .{});
        defer file.close(s.io);
        const st = try file.stat(s.io);
        if (st.kind != .file) return error.NotRegularFile;
        if (!mp4.probe(file, s.io)) return error.UnsupportedFormat;
        var random: [16]u8 = undefined;
        s.io.random(&random);
        const src = try s.a.create(Source);
        errdefer s.a.destroy(src);
        const source_id = try a.dupe(u8, &std.fmt.bytesToHex(random, .lower));
        const source_name = try a.dupe(u8, name);
        src.* = .{ .arena = arena, .id = source_id, .path = abs, .name = source_name, .size = st.size, .mtime = st.mtime.nanoseconds, .tables = .init(a), .frames = .init(a) };
        // Allocator handles point at the arena object, so initialize the maps
        // after the arena has reached its stable address in Source.
        src.tables = .init(src.arena.allocator());
        src.frames = .init(src.arena.allocator());
        try s.mutex.lock(s.io);
        defer s.mutex.unlock(s.io);
        for (s.sources.items) |existing| {
            if (std.mem.eql(u8, existing.path, abs)) {
                try existing.unchanged(s.io);
                src.arena.deinit();
                s.a.destroy(src);
                return;
            }
        }
        if (s.sources.items.len >= 16) return error.SourceLimit;
        try s.sources.append(s.a, src);
    }
    fn deinit(s: *Server) void {
        for (s.sources.items) |src| {
            src.arena.deinit();
            s.a.destroy(src);
        }
        s.sources.deinit(s.a);
        for (s.retired.items) |src| {
            src.arena.deinit();
            s.a.destroy(src);
        }
        s.retired.deinit(s.a);
        s.runtime.deinit();
    }
    fn tables(s: *Server, a: A, c: t.Control, name: []const u8, filename: ?[]const u8) !t.Tables {
        if (mp4.schema(name) == null) return error.UnknownTable;
        const selected = if (filename) |path| blk: {
            try s.add(path, path);
            break :blk try std.Io.Dir.cwd().realPathFileAlloc(s.io, path, a);
        } else null;
        var parts: std.ArrayList(t.Table) = .empty;
        try s.mutex.lock(s.io);
        defer s.mutex.unlock(s.io);
        if (s.sources.items.len == 0) return error.LoadVideoFirst;
        for (s.sources.items) |src| {
            if (selected) |path| {
                if (!std.mem.eql(u8, src.path, path)) continue;
            }
            try src.unchanged(s.io);
            if (!src.tables.contains(name)) {
                const alloc = src.arena.allocator();
                const all = try mp4.read(alloc, c, src.path, src.name, name);
                var it = all.iterator();
                while (it.next()) |entry| {
                    if (src.tables.contains(entry.key_ptr.*)) continue;
                    var table = entry.value_ptr.*;
                    if (std.mem.eql(u8, entry.key_ptr.*, "frames")) {
                        const wrapper = try alloc.create(FrameTable);
                        wrapper.* = .{ .server = s, .source = src, .base = table };
                        table.context = wrapper;
                        table.get = FrameTable.get;
                    }
                    try src.tables.put(entry.key_ptr.*, table);
                }
            }
            try src.unchanged(s.io);
            try parts.append(a, src.tables.get(name).?);
        }
        var out = t.Tables.init(a);
        try out.put(name, try t.combine(a, parts.items));
        return out;
    }
    fn lookup(s: *Server, id: []const u8) !t.Frame {
        try s.mutex.lock(s.io);
        defer s.mutex.unlock(s.io);
        for (s.sources.items) |src| {
            if (src.frames.get(id)) |f| {
                try src.unchanged(s.io);
                return f;
            }
        }
        return error.UnknownFrame;
    }
    fn info(s: *Server, a: A, request: *Request) !void {
        const Item = struct { id: []const u8, name: []const u8, size: u64, frames: ?usize };
        var items: std.ArrayList(Item) = .empty;
        {
            try s.mutex.lock(s.io);
            defer s.mutex.unlock(s.io);
            for (s.sources.items) |src| try items.append(a, .{ .id = src.id, .name = src.name, .size = src.size, .frames = if (src.tables.get("frames")) |f| f.count else null });
        }
        const available = decoderAvailable();
        try respondJSON(a, request, .{ .sources = items.items, .schemas = mp4.schemas, .decoder_available = available });
    }
    fn remove(s: *Server, a: A, request: *Request, id: []const u8) !void {
        try s.mutex.lock(s.io);
        defer s.mutex.unlock(s.io);
        for (s.sources.items, 0..) |src, i| {
            if (std.mem.eql(u8, src.id, id)) {
                try s.retired.append(s.a, src);
                _ = s.sources.orderedRemove(i);
                try respondJSON(a, request, .{ .removed = true });
                return;
            }
        }
        return error.UnknownSource;
    }
    fn executeQuery(s: *Server, a: A, c: t.Control, q: engine.Query, emit: ?engine.Emit) !t.Result {
        if (q.create_index) |name| if (emit) |out| try out.report(.{ .index = name, .completed = 0, .total = 0, .phase = .preparing });
        const input_tables = if (q.show_indexes) t.Tables.init(a) else try s.tables(a, c, q.source(), q.sourceFile());
        if (q.create_index) |name| if (emit) |out| try out.report(.{ .index = name, .completed = 0, .total = 0, .phase = .waiting });
        // Only native model and decoder operations share the inference lock.
        // Source metadata and raw downloads remain accessible during inference.
        try lockWithControl(&s.inference_mutex, c);
        defer s.inference_mutex.unlock(s.io);
        const functions = try s.runtime.functions(a);
        return engine.execute(a, c, q, input_tables, functions, emit);
    }
    fn query(s: *Server, a: A, request: *Request, c: t.Control, streaming: bool) !void {
        const input = try readJSON(struct { sql: []const u8 }, a, request);
        const q = try engine.parse(a, input.sql);
        setQueryDeadline(c, q);
        if (!streaming) {
            const result = try s.executeQuery(a, c, q, null);
            // Keep the original web API's truncated field.
            const bytes = try std.json.Stringify.valueAlloc(a, result, .{});
            const body = try std.fmt.allocPrint(a, "{s},\"truncated\":false}}", .{bytes[0 .. bytes.len - 1]});
            try respond(request, body, "application/json", .ok);
            return;
        }
        var buffer: [8192]u8 = undefined;
        var body = try request.respondStreaming(&buffer, .{ .respond_options = options("application/x-ndjson", .ok) });
        const Stream = struct {
            a: A,
            body: *std.http.BodyWriter,
            columns: []const []const u8 = &.{},
            fn progress(ctx: *anyopaque, value: engine.Progress) !void {
                const stream: *@This() = @ptrCast(@alignCast(ctx));
                try stream.write(.{ .type = "progress", .index = value.index, .completed = value.completed, .total = value.total, .phase = value.phase });
            }
            fn send(ctx: *anyopaque, result: t.Result, schema: bool) !void {
                const stream: *@This() = @ptrCast(@alignCast(ctx));
                if (schema) {
                    stream.columns = result.columns;
                    try stream.write(.{ .type = "schema", .columns = result.columns });
                } else {
                    const json = try std.json.Stringify.valueAlloc(stream.a, result, .{});
                    defer stream.a.free(json);
                    // The engine already serializes named rows consistently.
                    const rows_start = std.mem.indexOf(u8, json, "\"rows\":").? + 7;
                    try stream.body.writer.writeAll("{\"type\":\"rows\",\"rows\":");
                    try stream.body.writer.writeAll(json[rows_start .. json.len - 1]);
                    try stream.body.writer.writeAll("}\n");
                    try stream.body.writer.flush();
                    try stream.body.flush();
                }
            }
            fn write(stream: *@This(), value: anytype) !void {
                const json = try std.json.Stringify.valueAlloc(stream.a, value, .{});
                defer stream.a.free(json);
                try stream.body.writer.writeAll(json);
                try stream.body.writer.writeByte('\n');
                try stream.body.writer.flush();
                try stream.body.flush();
            }
        };
        var out = Stream{ .a = a, .body = &body };
        _ = s.executeQuery(a, c, q, .{ .context = &out, .send = Stream.send, .progress = Stream.progress }) catch |err| {
            out.write(.{ .type = "error", .@"error" = @errorName(err) }) catch {};
            body.end() catch {};
            return;
        };
        try out.write(.{ .type = "done", .truncated = false });
        try body.end();
    }
    fn render(s: *Server, a: A, request: *Request, c: t.Control) !void {
        setDeadline(c);
        const input = try readJSON(struct { ids: []const []const u8 }, a, request);
        if (input.ids.len == 0 or input.ids.len > 4096) return error.InvalidRenderBatch;
        try lockWithControl(&s.inference_mutex, c);
        defer s.inference_mutex.unlock(s.io);
        for (input.ids) |id| _ = try s.runtime.decode(a, c, try s.lookup(id), .preview);
        try respondJSON(a, request, .{ .ready = true });
    }
    fn frameResponse(s: *Server, a: A, request: *Request, c: t.Control, id: []const u8, raw: bool) !void {
        const f = try s.lookup(id);
        if (raw) {
            const file = try std.Io.Dir.cwd().openFile(s.io, f.source, .{});
            defer file.close(s.io);
            var range: ?ByteRange = null;
            var headers_it = request.iterateHeaders();
            while (headers_it.next()) |h| {
                if (std.ascii.eqlIgnoreCase(h.name, "range")) {
                    range = parseRange(h.value, f.size) catch {
                        const headers = [_]std.http.Header{.{ .name = "content-range", .value = try std.fmt.allocPrint(a, "bytes */{d}", .{f.size}) }};
                        return request.respond("Invalid byte range", .{ .status = .range_not_satisfiable, .keep_alive = false, .extra_headers = &headers });
                    };
                }
            }
            const selected = range orelse ByteRange{ .start = 0, .length = f.size };
            var headers: std.ArrayList(std.http.Header) = .empty;
            try headers.appendSlice(a, options("application/octet-stream", .ok).extra_headers);
            try headers.append(a, .{ .name = "accept-ranges", .value = "bytes" });
            try headers.append(a, .{ .name = "content-disposition", .value = try std.fmt.allocPrint(a, "attachment; filename=\"track-{d}-frame-{d}.bin\"", .{ f.track_id, f.frame_index }) });
            if (range != null) try headers.append(a, .{ .name = "content-range", .value = try std.fmt.allocPrint(a, "bytes {d}-{d}/{d}", .{ selected.start, selected.start + selected.length - 1, f.size }) });
            var buffer: [8192]u8 = undefined;
            var body = try request.respondStreaming(&buffer, .{ .content_length = selected.length, .respond_options = .{ .status = if (range == null) .ok else .partial_content, .keep_alive = false, .extra_headers = headers.items } });
            var bytes: [65536]u8 = undefined;
            var offset: u64 = 0;
            while (offset < selected.length) {
                try c.check();
                const n = @min(bytes.len, selected.length - offset);
                try t.readAt(file, s.io, bytes[0..n], f.offset + selected.start + offset);
                try body.writer.writeAll(bytes[0..n]);
                offset += n;
            }
            try body.end();
        } else {
            setDeadline(c);
            try lockWithControl(&s.inference_mutex, c);
            const path = s.runtime.decode(a, c, f, .preview) catch |err| {
                s.inference_mutex.unlock(s.io);
                return err;
            };
            s.inference_mutex.unlock(s.io);
            const bytes = try std.Io.Dir.cwd().readFileAlloc(s.io, path, a, .limited(32 << 20));
            try respond(request, bytes, "image/jpeg", .ok);
        }
    }
    fn upload(s: *Server, a: A, request: *Request, c: t.Control) !void {
        const content_type = request.head.content_type orelse return error.ExpectedMultipart;
        if (!std.mem.startsWith(u8, content_type, "multipart/form-data;")) return error.ExpectedMultipart;
        const boundary_pos = std.mem.indexOf(u8, content_type, "boundary=") orelse return error.InvalidMultipart;
        const boundary = try a.dupe(u8, std.mem.trim(u8, content_type[boundary_pos + 9 ..], "\" "));
        if (boundary.len == 0 or boundary.len > 200 or std.mem.indexOfAny(u8, boundary, "\r\n") != null) return error.InvalidMultipart;
        var buffer: [8192]u8 = undefined;
        const reader = try request.readerExpectContinue(&buffer);
        const start = (try reader.takeDelimiter('\n')) orelse return error.InvalidMultipart;
        const expected = try std.fmt.allocPrint(a, "--{s}\r", .{boundary});
        if (!std.mem.eql(u8, start, expected)) return error.InvalidMultipart;
        var filename: ?[]const u8 = null;
        var header_size: usize = 0;
        while (true) {
            const line = (try reader.takeDelimiter('\n')) orelse return error.InvalidMultipart;
            header_size += line.len;
            if (header_size > 8192) return error.MultipartHeadersTooLarge;
            if (std.mem.eql(u8, line, "\r")) break;
            if (std.ascii.startsWithIgnoreCase(line, "content-disposition:")) {
                if (std.mem.indexOf(u8, line, "name=\"file\"") == null) return error.ExpectedFileUpload;
                const begin = (std.mem.indexOf(u8, line, "filename=\"") orelse return error.ExpectedFileUpload) + 10;
                const end = std.mem.indexOfScalarPos(u8, line, begin, '"') orelse return error.InvalidMultipart;
                filename = try a.dupe(u8, std.fs.path.basename(line[begin..end]));
            }
        }
        const name = filename orelse return error.ExpectedFileUpload;
        if (name.len == 0) return error.ExpectedFileUpload;
        var random: [16]u8 = undefined;
        s.io.random(&random);
        const path = try std.fmt.allocPrint(a, "{s}/upload-{s}.mp4", .{ s.runtime.temp, std.fmt.bytesToHex(random, .lower) });
        const file = try std.Io.Dir.cwd().createFile(s.io, path, .{});
        defer file.close(s.io);
        var retained = false;
        defer if (!retained) {
            std.Io.Dir.cwd().deleteFile(s.io, path) catch {};
        };
        const marker = try std.fmt.allocPrint(a, "\r\n--{s}", .{boundary});
        var pending: std.ArrayList(u8) = .empty;
        var data: [65536]u8 = undefined;
        var found = false;
        while (true) {
            try c.check();
            const n = try reader.readSliceShort(&data);
            if (n == 0) break;
            try pending.appendSlice(a, data[0..n]);
            if (std.mem.indexOf(u8, pending.items, marker)) |pos| {
                try file.writeStreamingAll(s.io, pending.items[0..pos]);
                const tail = try reader.allocRemaining(a, .limited(8192));
                try pending.appendSlice(a, tail);
                if (!std.mem.eql(u8, std.mem.trim(u8, pending.items[pos + marker.len ..], "\r\n"), "--")) return error.UploadOneFileOnly;
                found = true;
                break;
            }
            const keep = @min(marker.len + 4, pending.items.len);
            const write_len = pending.items.len - keep;
            try file.writeStreamingAll(s.io, pending.items[0..write_len]);
            std.mem.copyForwards(u8, pending.items[0..keep], pending.items[write_len..]);
            pending.items.len = keep;
        }
        if (!found) return error.InvalidMultipart;
        try s.add(path, name);
        retained = true;
        try respondJSON(a, request, .{ .loaded = true });
    }
    fn route(s: *Server, a: A, request: *Request, c: t.Control) !void {
        try validateOrigin(request);
        const target = try a.dupe(u8, request.head.target);
        const method = request.head.method;
        if (method == .GET and std.mem.eql(u8, target, "/api/info")) return s.info(a, request);
        if (method == .POST and std.mem.eql(u8, target, "/api/query")) return s.query(a, request, c, false);
        if (method == .POST and std.mem.eql(u8, target, "/api/query/stream")) return s.query(a, request, c, true);
        if (method == .POST and std.mem.eql(u8, target, "/api/source")) {
            const input = try readJSON(struct { path: []const u8 }, a, request);
            try s.add(input.path, std.fs.path.basename(input.path));
            return respondJSON(a, request, .{ .loaded = true });
        }
        if (method == .POST and std.mem.eql(u8, target, "/api/upload")) return s.upload(a, request, c);
        if (method == .POST and std.mem.eql(u8, target, "/api/render")) return s.render(a, request, c);
        if (method == .DELETE and std.mem.startsWith(u8, target, "/api/source/")) return s.remove(a, request, target[12..]);
        if (method == .GET and std.mem.startsWith(u8, target, "/api/frame/")) {
            const tail = target[11..];
            const slash = std.mem.indexOfScalar(u8, tail, '/') orelse return error.UnknownRoute;
            const action = tail[slash + 1 ..];
            if (!std.mem.eql(u8, action, "raw") and !std.mem.eql(u8, action, "preview")) return error.UnknownRoute;
            return s.frameResponse(a, request, c, tail[0..slash], std.mem.eql(u8, action, "raw"));
        }
        if (method == .GET or method == .HEAD) {
            inline for (.{ .{ "/", "index.html", "text/html" }, .{ "/app.js", "app.js", "text/javascript" }, .{ "/graph.js", "graph.js", "text/javascript" }, .{ "/masks.js", "masks.js", "text/javascript" }, .{ "/style.css", "style.css", "text/css" } }) |asset| {
                if (std.mem.eql(u8, target, asset[0])) return respond(request, @embedFile("static/" ++ asset[1]), asset[2], .ok);
            }
        }
        return error.UnknownRoute;
    }
};
fn readJSON(comptime T: type, a: A, request: *Request) !T {
    const ct = request.head.content_type orelse return error.ExpectedJSON;
    if (!std.mem.startsWith(u8, ct, "application/json")) return error.ExpectedJSON;
    var buffer: [8192]u8 = undefined;
    const reader = try request.readerExpectContinue(&buffer);
    const bytes = try reader.allocRemaining(a, .limited(1 << 20));
    const parsed = try std.json.parseFromSlice(T, a, bytes, .{ .allocate = .alloc_always });
    return parsed.value;
}
fn options(comptime content_type: []const u8, comptime status: std.http.Status) Request.RespondOptions {
    // Constructed by respond below, which supplies the stack-backed headers.
    return .{ .status = status, .keep_alive = false, .extra_headers = &.{ .{ .name = "content-type", .value = content_type }, .{ .name = "x-content-type-options", .value = "nosniff" }, .{ .name = "cache-control", .value = "no-store" } } };
}
fn respond(request: *Request, bytes: []const u8, content_type: []const u8, status: std.http.Status) !void {
    const headers = [_]std.http.Header{
        .{ .name = "content-type", .value = content_type },                                                                                                                          .{ .name = "x-content-type-options", .value = "nosniff" },
        .{ .name = "content-security-policy", .value = "default-src 'self'; img-src 'self' blob:; style-src 'self'; script-src 'self'; object-src 'none'; frame-ancestors 'none'" },
    };
    try request.respond(bytes, .{ .status = status, .keep_alive = false, .extra_headers = &headers });
}
fn respondJSON(a: A, request: *Request, value: anytype) !void {
    try respond(request, try std.json.Stringify.valueAlloc(a, value, .{}), "application/json", .ok);
}
fn validateOrigin(request: *Request) !void {
    var host: ?[]const u8 = null;
    var origin: ?[]const u8 = null;
    var fetch: ?[]const u8 = null;
    var it = request.iterateHeaders();
    while (it.next()) |h| {
        if (std.ascii.eqlIgnoreCase(h.name, "host")) host = h.value;
        if (std.ascii.eqlIgnoreCase(h.name, "origin")) origin = h.value;
        if (std.ascii.eqlIgnoreCase(h.name, "sec-fetch-site")) fetch = h.value;
    }
    const h = host orelse return error.Forbidden;
    const colon = std.mem.indexOfScalar(u8, h, ':');
    const hostname = h[0 .. colon orelse h.len];
    if (!std.mem.eql(u8, hostname, "localhost") and !std.mem.eql(u8, hostname, "127.0.0.1")) return error.Forbidden;
    if (fetch) |f| {
        if (std.mem.eql(u8, f, "cross-site")) return error.Forbidden;
    }
    if (origin) |o| {
        if (!std.mem.startsWith(u8, o, "http://") or !std.mem.eql(u8, o[7..], h)) return error.Forbidden;
    }
}
fn lockWithControl(mutex: *std.Io.Mutex, c: t.Control) !void {
    while (!mutex.tryLock()) {
        try c.check();
        try std.Io.sleep(c.io, .fromMilliseconds(10), .awake);
    }
    c.check() catch |err| {
        mutex.unlock(c.io);
        return err;
    };
}
fn decoderAvailable() bool {
    const paths = t.env("PATH") orelse return false;
    var have_ffmpeg = false;
    var have_ffprobe = false;
    var it = std.mem.splitScalar(u8, paths, ':');
    var buf: [4096]u8 = undefined;
    while (it.next()) |dir| {
        const p1 = std.fmt.bufPrintSentinel(&buf, "{s}/ffmpeg", .{dir}, 0) catch continue;
        if (std.c.access(p1, 1) == 0) have_ffmpeg = true;
        const p2 = std.fmt.bufPrintSentinel(&buf, "{s}/ffprobe", .{dir}, 0) catch continue;
        if (std.c.access(p2, 1) == 0) have_ffprobe = true;
    }
    return have_ffmpeg and have_ffprobe;
}
fn connection(s: *Server, stream: std.Io.net.Stream) void {
    defer stream.close(s.io);
    control_mod.timeout(stream.socket.handle);
    var arena = std.heap.ArenaAllocator.init(s.a);
    defer arena.deinit();
    const a = arena.allocator();
    var in_buf: [16384]u8 = undefined;
    var out_buf: [8192]u8 = undefined;
    var reader = stream.reader(s.io, &in_buf);
    var writer = stream.writer(s.io, &out_buf);
    var http = std.http.Server.init(&reader.interface, &writer.interface);
    var request = http.receiveHead() catch return;
    var check = control_mod.RequestControl{ .fd = stream.socket.handle, .io = s.io };
    const c = t.Control{ .io = s.io, .check_context = &check, .check_fn = control_mod.RequestControl.canceled };
    s.route(a, &request, c) catch |err| {
        const status: std.http.Status = switch (err) {
            error.Forbidden => .forbidden,
            error.UnknownRoute, error.UnknownFrame, error.UnknownSource, error.FileNotFound => .not_found,
            error.SourceChanged => .conflict,
            else => .bad_request,
        };
        const json = std.json.Stringify.valueAlloc(a, .{ .@"error" = @errorName(err) }, .{}) catch return;
        respond(&request, json, "application/json", status) catch {};
    };
}
pub fn run(init: std.process.Init) !void {
    control_mod.install();
    var arena = std.heap.ArenaAllocator.init(init.gpa);
    defer arena.deinit();
    const a = arena.allocator();
    const args = try init.minimal.args.toSlice(a);
    var port: u16 = 3000;
    var i: usize = 1;
    if (i < args.len and (std.mem.eql(u8, args[i], "-h") or std.mem.eql(u8, args[i], "--help"))) {
        std.debug.print("Usage: bquery-web [-port PORT] [FILE ...]\n", .{});
        return;
    }
    while (i < args.len and std.mem.startsWith(u8, args[i], "-")) {
        if (std.mem.eql(u8, args[i], "--")) {
            i += 1;
            break;
        }
        const flag = if (std.mem.startsWith(u8, args[i], "--")) args[i][1..] else args[i];
        if (std.mem.startsWith(u8, flag, "-port=")) {
            port = try std.fmt.parseInt(u16, flag[6..], 10);
            i += 1;
        } else {
            if (!std.mem.eql(u8, flag, "-port") or i + 1 >= args.len) return error.InvalidArgument;
            port = try std.fmt.parseInt(u16, args[i + 1], 10);
            i += 2;
        }
    }
    var server = Server{ .a = init.gpa, .io = init.io, .runtime = try video.Runtime.init(init.gpa, init.io) };
    defer server.deinit();
    for (args[i..]) |path| try server.add(path, std.fs.path.basename(path));
    const address: std.Io.net.IpAddress = .{ .ip4 = .loopback(port) };
    var listener = try address.listen(init.io, .{ .reuse_address = true });
    defer listener.deinit(init.io);
    try std.Io.File.stdout().writeStreamingAll(init.io, try std.fmt.allocPrint(a, "Binary Query: http://127.0.0.1:{d}\n", .{listener.socket.address.getPort()}));
    var group: std.Io.Group = .init;
    defer group.cancel(init.io);
    while (!control_mod.stopped()) {
        if (!control_mod.ready(listener.socket.handle)) {
            try std.Io.sleep(init.io, .fromMilliseconds(25), .awake);
            continue;
        }
        const stream = try listener.accept(init.io);
        group.concurrent(init.io, connection, .{ &server, stream }) catch {
            stream.close(init.io);
            return error.ConnectionLimit;
        };
    }
}
test "web tables match exported MP4 schemas" {
    try std.testing.expectEqual(@as(usize, 10), mp4.schemas.frames.len);
}
fn setDeadline(c: t.Control) void {
    const check: *control_mod.RequestControl = @ptrCast(@alignCast(c.check_context.?));
    check.deadline_ns = std.Io.Timestamp.now(c.io, .awake).nanoseconds + 120 * std.time.ns_per_s;
}

fn setQueryDeadline(c: t.Control, q: engine.Query) void {
    if (!q.uses(&.{ "yolo", "person_count", "object_count", "sam3", "sam3_count", "sam3_score" })) setDeadline(c);
}

test "YOLO inference has no query deadline while metadata queries remain bounded" {
    var arena = std.heap.ArenaAllocator.init(std.testing.allocator);
    defer arena.deinit();
    const a = arena.allocator();
    for ([_][]const u8{
        "SELECT yolo(frame, 'car') AS cars FROM frames('video.mp4')",
        "SELECT frame FROM frames('video.mp4') WHERE yolo(frame, 'car') > 0",
        "CREATE INDEX people ON frames('video.mp4') (yolo(frame, 'person'))",
    }) |sql| {
        var check = control_mod.RequestControl{ .io = std.testing.io };
        const c = t.Control{ .io = check.io, .check_context = &check, .check_fn = control_mod.RequestControl.canceled };
        setQueryDeadline(c, try engine.parse(a, sql));
        try std.testing.expectEqual(@as(?i96, null), check.deadline_ns);
    }
    var check = control_mod.RequestControl{ .io = std.testing.io };
    const c = t.Control{ .io = check.io, .check_context = &check, .check_fn = control_mod.RequestControl.canceled };
    setQueryDeadline(c, try engine.parse(a, "SELECT frame_index FROM frames('video.mp4')"));
    try std.testing.expect(check.deadline_ns != null);
}

const ByteRange = struct { start: u64, length: u64 };
fn parseRange(value: []const u8, size: u64) !?ByteRange {
    // HTTP permits ignoring unsupported range units and multipart ranges.
    if (!std.mem.startsWith(u8, value, "bytes=") or std.mem.indexOfScalar(u8, value, ',') != null) return null;
    const spec = std.mem.trim(u8, value[6..], " \t");
    const dash = std.mem.indexOfScalar(u8, spec, '-') orelse return error.InvalidRange;
    if (size == 0) return error.InvalidRange;
    if (dash == 0) {
        const suffix = try std.fmt.parseInt(u64, spec[1..], 10);
        if (suffix == 0) return error.InvalidRange;
        const length = @min(suffix, size);
        return .{ .start = size - length, .length = length };
    }
    const start = try std.fmt.parseInt(u64, spec[0..dash], 10);
    if (start >= size) return error.InvalidRange;
    const end = if (dash + 1 == spec.len) size - 1 else @min(size - 1, try std.fmt.parseInt(u64, spec[dash + 1 ..], 10));
    if (end < start) return error.InvalidRange;
    return .{ .start = start, .length = end - start + 1 };
}
test "raw sample byte ranges are relative to the encoded sample" {
    try std.testing.expectEqual(ByteRange{ .start = 2, .length = 3 }, (try parseRange("bytes=2-4", 10)).?);
    try std.testing.expectEqual(ByteRange{ .start = 8, .length = 2 }, (try parseRange("bytes=-2", 10)).?);
    try std.testing.expectEqual(ByteRange{ .start = 2, .length = 8 }, (try parseRange("bytes=2-", 10)).?);
    try std.testing.expectEqual(ByteRange{ .start = 0, .length = 10 }, (try parseRange("bytes=-20", 10)).?);
    try std.testing.expectError(error.InvalidRange, parseRange("bytes=10-", 10));
    try std.testing.expectError(error.InvalidRange, parseRange("bytes=4-2", 10));
}
