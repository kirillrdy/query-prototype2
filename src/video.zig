const std = @import("std");
const sam3 = @import("sam3");
const t = @import("types.zig");
const engine = @import("engine.zig");
const ort = @import("ort.zig");
const A = std.mem.Allocator;
const V = t.Value;
const File = std.Io.File;
extern "c" fn waitpid(c_int, *c_int, c_int) c_int;
const Representation = enum { preview, gray, analysis, detection };
fn suffix(r: Representation) []const u8 {
    return switch (r) {
        .preview => ".jpg",
        .gray => ".gray",
        .analysis => ".png",
        .detection => ".detect.jpg",
    };
}
pub const Mask = struct { score: f32, rle: []const u32 };
pub const Segmentation = struct { count: usize, score: f32, width: usize, height: usize, masks: []const Mask };
const Signature = struct { size: u64, mtime: i96 };
const SamCache = struct { arena: std.heap.ArenaAllocator, result: Segmentation };
const Counts = [80]u32;
pub const Runtime = struct {
    a: A,
    io: std.Io,
    temp: []const u8,
    counter: usize = 0,
    signatures: std.StringHashMap(Signature),
    indices: std.StringHashMap(std.AutoHashMap(u64, usize)),
    model: ?sam3.Model = null,
    embedding: ?sam3.TextImageEmbedding = null,
    embedding_key: ?[]const u8 = null,
    features: std.StringHashMap([]f32),
    results: std.StringHashMap(SamCache),
    detector: ?ort.Detector = null,
    counts: std.StringHashMap(Counts),
    pub fn init(a: A, io: std.Io) !Runtime {
        var random: [16]u8 = undefined;
        io.random(&random);
        const temp = try std.fmt.allocPrint(a, "/tmp/binaryquery-{s}", .{std.fmt.bytesToHex(random, .lower)});
        errdefer a.free(temp);
        try std.Io.Dir.cwd().createDirPath(io, temp);
        return .{ .a = a, .io = io, .temp = temp, .signatures = .init(a), .indices = .init(a), .features = .init(a), .results = .init(a), .counts = .init(a) };
    }
    pub fn deinit(r: *Runtime) void {
        if (r.embedding) |*e| e.deinit();
        if (r.embedding_key) |key| r.a.free(key);
        if (r.model) |*m| m.deinit();
        if (r.detector) |*d| d.deinit();
        var features = r.features.iterator();
        while (features.next()) |entry| {
            r.a.free(entry.key_ptr.*);
            r.a.free(entry.value_ptr.*);
        }
        r.features.deinit();
        r.clearResults();
        r.results.deinit();
        var counts = r.counts.keyIterator();
        while (counts.next()) |key| r.a.free(key.*);
        r.counts.deinit();
        var indices = r.indices.iterator();
        while (indices.next()) |entry| {
            r.a.free(entry.key_ptr.*);
            entry.value_ptr.deinit();
        }
        r.indices.deinit();
        var sig = r.signatures.keyIterator();
        while (sig.next()) |key| r.a.free(key.*);
        r.signatures.deinit();
        std.Io.Dir.cwd().deleteTree(r.io, r.temp) catch {};
        r.a.free(r.temp);
    }
    fn clearResults(r: *Runtime) void {
        var it = r.results.iterator();
        while (it.next()) |entry| {
            r.a.free(entry.key_ptr.*);
            entry.value_ptr.arena.deinit();
        }
        r.results.clearRetainingCapacity();
    }
    pub fn functions(r: *Runtime, a: A) !engine.Functions {
        var f = engine.Functions.init(a);
        inline for (.{ .{ "motion", 2, motion }, .{ "person_count", 1, personCount }, .{ "object_count", 2, objectCount }, .{ "sam3", 2, segmentMasks }, .{ "sam3_count", 2, segmentCount }, .{ "sam3_score", 2, segmentScore } }) |entry| {
            try f.put(entry[0], .{ .arity = entry[1], .context = r, .evaluate = entry[2] });
        }
        f.getPtr("motion").?.prepare = prepareMotion;
        return f;
    }
    pub fn checkSource(r: *Runtime, frame: t.Frame) !void {
        const st = try std.Io.Dir.cwd().statFile(r.io, frame.source, .{});
        if (st.kind != .file) return error.NotRegularFile;
        const sig = Signature{ .size = st.size, .mtime = st.mtime.nanoseconds };
        if (r.signatures.get(frame.source)) |old| {
            if (old.size != sig.size or old.mtime != sig.mtime) return error.SourceChanged;
        } else try r.signatures.put(try r.a.dupe(u8, frame.source), sig);
    }
    pub fn id(a: A, f: t.Frame) ![]const u8 {
        var hash: [32]u8 = undefined;
        std.crypto.hash.sha2.Sha256.hash(f.source, &hash, .{});
        return std.fmt.allocPrint(a, "{s}-{d}-{d}", .{ std.fmt.bytesToHex(hash[0..16].*, .lower), f.track_id, f.frame_index });
    }
    pub fn path(r: *Runtime, a: A, frame: t.Frame, representation: Representation) ![]const u8 {
        const key = try id(a, frame);
        return std.fmt.allocPrint(a, "{s}/{s}{s}", .{ r.temp, key, suffix(representation) });
    }
    // stdout/stderr are files so the child cannot deadlock on a full pipe. Poll
    // with cancellation and size checks, then reap on every path.
    pub fn command(r: *Runtime, a: A, control: t.Control, argv: []const []const u8, limit: usize) ![]const u8 {
        try control.check();
        r.counter += 1;
        const out_path = try std.fmt.allocPrint(a, "{s}/command-{d}.out", .{ r.temp, r.counter });
        const err_path = try std.fmt.allocPrint(a, "{s}/command-{d}.err", .{ r.temp, r.counter });
        const out = try std.Io.Dir.cwd().createFile(r.io, out_path, .{});
        defer out.close(r.io);
        defer std.Io.Dir.cwd().deleteFile(r.io, out_path) catch {};
        const err = try std.Io.Dir.cwd().createFile(r.io, err_path, .{});
        defer err.close(r.io);
        defer std.Io.Dir.cwd().deleteFile(r.io, err_path) catch {};
        var child = try std.process.spawn(r.io, .{ .argv = argv, .stdin = .ignore, .stdout = .{ .file = out }, .stderr = .{ .file = err } });
        defer child.kill(r.io);
        var status: c_int = 0;
        while (true) {
            try control.check();
            const finished = waitpid(child.id.?, &status, 1);
            if (finished == child.id.?) {
                child.id = null;
                break;
            }
            if (finished < 0) return error.ChildWaitFailed;
            if ((try out.stat(r.io)).size > limit or (try err.stat(r.io)).size > 1 << 20) return error.CommandOutputTooLarge;
            try std.Io.sleep(r.io, .fromMilliseconds(10), .awake);
        }
        if (status != 0) {
            const message = try std.Io.Dir.cwd().readFileAlloc(r.io, err_path, a, .limited(1 << 20));
            std.log.err("{s}: {s}", .{ argv[0], message });
            return error.CommandFailed;
        }
        return std.Io.Dir.cwd().readFileAlloc(r.io, out_path, a, .limited(limit));
    }
    fn ordinal(r: *Runtime, a: A, control: t.Control, f: t.Frame) !usize {
        const start = if (f.seekable) @max(0, f.decode_start - 0.000001) else 0;
        const key = try std.fmt.allocPrint(a, "{s}:{d}:{d}:{any}", .{ f.source, f.track_id, start, f.keyframe });
        if (r.indices.get(key)) |index| {
            if (index.get(f.offset)) |n| return n;
        }
        var args: std.ArrayList([]const u8) = .empty;
        try args.appendSlice(a, &.{ "ffprobe", "-v", "error", "-ignore_editlist", "1" });
        if (start > 0) try args.appendSlice(a, &.{ "-read_intervals", try std.fmt.allocPrint(a, "{d:.6}%", .{start}) });
        if (f.seekable and f.keyframe) try args.appendSlice(a, &.{ "-skip_frame", "nokey" });
        try args.appendSlice(a, &.{ "-select_streams", try std.fmt.allocPrint(a, "i:{d}", .{f.track_id}), "-show_frames", "-show_entries", "frame=pkt_pos:frame_side_data=", "-of", "default=noprint_wrappers=1:nokey=1", f.source });
        var child = try std.process.spawn(r.io, .{ .argv = args.items, .stdin = .ignore, .stdout = .pipe, .stderr = .ignore });
        defer child.kill(r.io);
        var buffer: [8192]u8 = undefined;
        var reader = child.stdout.?.readerStreaming(r.io, &buffer);
        const entry = try r.indices.getOrPut(key);
        if (!entry.found_existing) {
            entry.key_ptr.* = try r.a.dupe(u8, key);
            entry.value_ptr.* = .init(r.a);
        }
        var n: usize = 0;
        while (try reader.interface.takeDelimiter('\n')) |line| {
            try control.check();
            const pos = std.fmt.parseInt(u64, std.mem.trim(u8, line, "\r \t"), 10) catch continue;
            if (entry.value_ptr.get(pos)) |existing| {
                if (existing != n) return error.AmbiguousFramePosition;
            }
            try entry.value_ptr.put(pos, n);
            if (pos == f.offset) return n;
            n += 1;
        }
        return error.FramePositionNotFound;
    }
    pub fn decode(r: *Runtime, a: A, control: t.Control, f: t.Frame, representation: Representation) ![]const u8 {
        try control.check();
        try r.checkSource(f);
        const dest = try r.path(a, f, representation);
        if (std.Io.Dir.cwd().access(r.io, dest, .{})) |_| return dest else |_| {}
        const n = try r.ordinal(a, control, f);
        var args: std.ArrayList([]const u8) = .empty;
        try args.appendSlice(a, &.{ "ffmpeg", "-v", "error", "-nostdin", "-ignore_editlist", "1" });
        const start = if (f.seekable) @max(0, f.decode_start - 0.000001) else 0;
        if (start > 0) try args.appendSlice(a, &.{ "-ss", try std.fmt.allocPrint(a, "{d:.6}", .{start}), "-seek_timestamp", "1", "-noaccurate_seek" });
        if (f.seekable and f.keyframe) try args.appendSlice(a, &.{ "-skip_frame", "nokey" });
        const filter = try std.fmt.allocPrint(a, "select=eq(n\\,{d}){s}", .{ n, if (representation == .gray) ",scale=160:90,format=gray" else ",scale=w='min(1280,iw)':h='min(720,ih)':force_original_aspect_ratio=decrease" });
        try args.appendSlice(a, &.{ "-i", f.source, "-map", try std.fmt.allocPrint(a, "0:i:{d}", .{f.track_id}), "-an", "-vf", filter, "-fps_mode", "vfr", "-frames:v", "1" });
        if (representation == .gray) try args.appendSlice(a, &.{ "-c:v", "rawvideo", "-pix_fmt", "gray", "-f", "image2" }) else if (representation == .preview or representation == .detection) try args.appendSlice(a, &.{ "-q:v", "2" }) else try args.appendSlice(a, &.{ "-c:v", "png" });
        const staging = try std.fmt.allocPrint(a, "{s}.part{s}", .{ dest, suffix(representation) });
        defer std.Io.Dir.cwd().deleteFile(r.io, staging) catch {};
        try args.appendSlice(a, &.{ "-y", staging });
        _ = try r.command(a, control, args.items, 1 << 20);
        try r.checkSource(f);
        try std.Io.Dir.cwd().rename(staging, .cwd(), dest, r.io);
        return dest;
    }
    fn prepareGray(r: *Runtime, a: A, control: t.Control, frames: []const t.Frame) !void {
        var needed: std.ArrayList(t.Frame) = .empty;
        var seen = std.StringHashMap(void).init(a);
        for (frames) |f| {
            try control.check();
            try r.checkSource(f);
            const dest = try r.path(a, f, .gray);
            if (std.Io.Dir.cwd().access(r.io, dest, .{})) |_| continue else |_| {}
            if (seen.contains(dest)) continue;
            try seen.put(dest, {});
            try needed.append(a, f);
        }
        const Group = struct {
            fn start(f: t.Frame) f64 {
                return if (f.seekable) @max(0, f.decode_start - 0.000001) else 0;
            }
            fn same(lhs: t.Frame, rhs: t.Frame) bool {
                return std.mem.eql(u8, lhs.source, rhs.source) and lhs.track_id == rhs.track_id and start(lhs) == start(rhs);
            }
            fn less(_: void, lhs: t.Frame, rhs: t.Frame) bool {
                const source = std.mem.order(u8, lhs.source, rhs.source);
                if (source != .eq) return source == .lt;
                if (lhs.track_id != rhs.track_id) return lhs.track_id < rhs.track_id;
                if (start(lhs) != start(rhs)) return start(lhs) < start(rhs);
                return lhs.pts_seconds < rhs.pts_seconds or (lhs.pts_seconds == rhs.pts_seconds and lhs.frame_index < rhs.frame_index);
            }
        };
        std.mem.sort(t.Frame, needed.items, {}, Group.less);
        var first: usize = 0;
        while (first < needed.items.len) {
            try control.check();
            var end = first + 1;
            while (end < needed.items.len and Group.same(needed.items[first], needed.items[end])) : (end += 1) {}
            const group = needed.items[first..end];
            // Probe the last requested picture first to populate the encoded
            // byte-position index for earlier pictures in the same seek range.
            var last = group[group.len - 1];
            last.keyframe = false;
            _ = try r.ordinal(a, control, last);
            const Picture = struct {
                frame: t.Frame,
                ordinal: usize,
                fn less(_: void, lhs: @This(), rhs: @This()) bool {
                    return lhs.ordinal < rhs.ordinal;
                }
            };
            const pictures = try a.alloc(Picture, group.len);
            for (group, pictures) |f, *picture| {
                var full = f;
                full.keyframe = false;
                picture.* = .{ .frame = f, .ordinal = try r.ordinal(a, control, full) };
            }
            std.mem.sort(Picture, pictures, {}, Picture.less);
            var filter: std.Io.Writer.Allocating = .init(a);
            try filter.writer.writeAll("select=");
            for (pictures, 0..) |picture, i| {
                if (i > 0) try filter.writer.writeByte('+');
                try filter.writer.print("eq(n\\,{d})", .{picture.ordinal});
            }
            try filter.writer.writeAll(",scale=160:90,format=gray");
            var args: std.ArrayList([]const u8) = .empty;
            try args.appendSlice(a, &.{ "ffmpeg", "-v", "error", "-nostdin", "-ignore_editlist", "1" });
            const start = Group.start(group[0]);
            if (start > 0) try args.appendSlice(a, &.{ "-ss", try std.fmt.allocPrint(a, "{d:.6}", .{start}), "-seek_timestamp", "1", "-noaccurate_seek" });
            try args.appendSlice(a, &.{ "-i", group[0].source, "-map", try std.fmt.allocPrint(a, "0:i:{d}", .{group[0].track_id}), "-an", "-vf", try filter.toOwnedSlice(), "-fps_mode", "vfr", "-frames:v", try std.fmt.allocPrint(a, "{d}", .{pictures.len}), "-c:v", "rawvideo", "-pix_fmt", "gray", "-f", "rawvideo", "pipe:1" });
            const pixels = 160 * 90;
            const bytes = try r.command(a, control, args.items, pictures.len * pixels + 1);
            if (bytes.len != pictures.len * pixels) return error.InvalidGrayBuffer;
            for (pictures, 0..) |picture, i| {
                try control.check();
                try r.checkSource(picture.frame);
                const dest = try r.path(a, picture.frame, .gray);
                const staging = try std.fmt.allocPrint(a, "{s}.part", .{dest});
                defer std.Io.Dir.cwd().deleteFile(r.io, staging) catch {};
                {
                    const file = try std.Io.Dir.cwd().createFile(r.io, staging, .{});
                    defer file.close(r.io);
                    try file.writeStreamingAll(r.io, bytes[i * pixels ..][0..pixels]);
                }
                try std.Io.Dir.cwd().rename(staging, .cwd(), dest, r.io);
            }
            first = end;
        }
    }
    fn segment(r: *Runtime, a: A, control: t.Control, f: t.Frame, phrase: []const u8) !Segmentation {
        try control.check();
        try r.checkSource(f);
        const threshold = try confidence("BQUERY_SAM3_CONFIDENCE", 0.5);
        const frame_key = try id(a, f);
        const key = try std.fmt.allocPrint(a, "{s}:{s}", .{ frame_key, phrase });
        if (r.results.get(key)) |cached| return cached.result;
        if (r.model == null) r.model = try sam3.Model.open(r.a, r.io);
        if (r.embedding_key == null or !std.mem.eql(u8, r.embedding_key.?, frame_key)) {
            if (r.embedding) |*e| e.deinit();
            r.embedding = null;
            if (r.embedding_key) |k| r.a.free(k);
            r.embedding_key = null;
            const png = try r.decode(a, control, f, .analysis);
            defer std.Io.Dir.cwd().deleteFile(r.io, png) catch {};
            const bytes = try std.Io.Dir.cwd().readFileAlloc(r.io, png, a, .limited(32 << 20));
            var image = try sam3.decodeImage(a, bytes);
            defer image.deinit(a);
            r.embedding = try r.model.?.encodeForText(sam3.RgbImage.fromImage(image));
            r.embedding_key = try r.a.dupe(u8, frame_key);
        }
        try control.check();
        if (!r.features.contains(phrase)) {
            if (r.features.count() >= 32) {
                var it = r.features.iterator();
                while (it.next()) |entry| {
                    r.a.free(entry.key_ptr.*);
                    r.a.free(entry.value_ptr.*);
                }
                r.features.clearRetainingCapacity();
            }
            const features = try r.model.?.encodeTextFeatures(phrase);
            errdefer r.a.free(features);
            try r.features.put(try r.a.dupe(u8, phrase), features);
        }
        var masks = try r.model.?.findWithTextFeatures(&r.embedding.?, phrase, r.features.get(phrase).?, .{ .min_score = threshold });
        defer masks.deinit();
        try control.check();
        try r.checkSource(f);
        if (r.results.count() >= 32) r.clearResults();
        var arena = std.heap.ArenaAllocator.init(r.a);
        errdefer arena.deinit();
        const alloc = arena.allocator();
        const output = try alloc.alloc(Mask, masks.count);
        for (output, 0..) |*mask, i| {
            var runs: std.ArrayList(u32) = .empty;
            var foreground = false;
            var length: u32 = 0;
            for (masks.plane(i)) |logit| {
                const next = logit > 0;
                if (next != foreground) {
                    try runs.append(alloc, length);
                    length = 0;
                    foreground = next;
                }
                length += 1;
            }
            try runs.append(alloc, length);
            mask.* = .{ .score = masks.scores[i], .rle = try runs.toOwnedSlice(alloc) };
        }
        const result = Segmentation{ .count = masks.count, .score = masks.object_score, .width = masks.width, .height = masks.height, .masks = output };
        try r.results.put(try r.a.dupe(u8, key), .{ .arena = arena, .result = result });
        return result;
    }
    fn detect(r: *Runtime, a: A, control: t.Control, f: t.Frame) !Counts {
        try r.checkSource(f);
        try control.check();
        const key = try id(a, f);
        if (r.counts.get(key)) |cached| return cached;
        const threshold = try confidence("BQUERY_DETECTION_CONFIDENCE", 0.25);
        if (r.detector == null) {
            const model = if (t.env("BQUERY_DETECTION_MODEL")) |path_| path_ else try r.modelPath(a, control);
            if (!std.ascii.endsWithIgnoreCase(model, ".onnx")) return error.ExpectedONNXModel;
            r.detector = try ort.Detector.open(a, r.io, model, try @import("detection_assets.zig").runtimePath(r, a, control));
        }
        const png = try r.decode(a, control, f, .detection);
        defer std.Io.Dir.cwd().deleteFile(r.io, png) catch {};
        const bytes = try std.Io.Dir.cwd().readFileAlloc(r.io, png, a, .limited(32 << 20));
        var image = try sam3.decodeImage(a, bytes);
        defer image.deinit(a);
        const input = try letterbox(a, sam3.RgbImage.fromImage(image), 640);
        defer a.free(input);
        const output = try r.detector.?.run(a, input);
        defer a.free(output.data);
        defer a.free(output.shape);
        const counts = try detectionCounts(a, output.data, output.shape, threshold);
        try control.check();
        try r.checkSource(f);
        if (r.counts.count() >= 4096) {
            var it = r.counts.keyIterator();
            while (it.next()) |k| r.a.free(k.*);
            r.counts.clearRetainingCapacity();
        }
        try r.counts.put(try r.a.dupe(u8, key), counts);
        return counts;
    }
    fn modelPath(r: *Runtime, a: A, control: t.Control) ![]const u8 {
        const hash = "634279b40c07c6391472c51ad45b81ebc48706a9a1fe72dd3396322acd0c053b";
        const cache = t.env("BQUERY_DETECTION_CACHE") orelse try std.fmt.allocPrint(a, "{s}/binaryquery/detection", .{t.env("XDG_CACHE_HOME") orelse try std.fmt.allocPrint(a, "{s}/.cache", .{t.env("HOME") orelse return error.HomeNotSet})});
        try std.Io.Dir.cwd().createDirPath(r.io, cache);
        const path_ = try std.fmt.allocPrint(a, "{s}/yolo11n-{s}.onnx", .{ cache, hash[0..12] });
        if (try validModel(a, r.io, path_, hash)) return path_;
        const staging = try std.fmt.allocPrint(a, "{s}.part", .{path_});
        defer std.Io.Dir.cwd().deleteFile(r.io, staging) catch {};
        _ = try r.command(a, control, &.{ "curl", "-fsSL", "--retry", "3", "--max-filesize", "33554432", "-o", staging, "https://github.com/ultralytics/assets/releases/download/v8.4.0/yolo11n.onnx" }, 1 << 20);
        if (!try validModel(a, r.io, staging, hash)) return error.ModelChecksumMismatch;
        try std.Io.Dir.cwd().rename(staging, .cwd(), path_, r.io);
        return path_;
    }
};
fn runtime(ctx: *anyopaque) *Runtime {
    return @ptrCast(@alignCast(ctx));
}
fn asFrame(v: V) !t.Frame {
    return if (v == .frame) v.frame else error.ExpectedFrame;
}
fn prompt(v: V) ![]const u8 {
    if (v != .string) return error.ExpectedTextPrompt;
    const s = std.mem.trim(u8, v.string, " \t\r\n");
    if (s.len == 0 or s.len > 4096) return error.InvalidTextPrompt;
    return s;
}
fn confidence(name: [:0]const u8, fallback: f32) !f32 {
    const n = if (t.env(name)) |s| std.fmt.parseFloat(f32, s) catch return error.InvalidConfidence else fallback;
    if (!std.math.isFinite(n) or n <= 0 or n > 1) return error.InvalidConfidence;
    return n;
}
fn segmentMasks(ctx: *anyopaque, a: A, control: t.Control, args: []const V) !V {
    if (args[0] == .null or args[1] == .null) return .null;
    const result = try runtime(ctx).segment(a, control, try asFrame(args[0]), try prompt(args[1]));
    return .{ .json = try std.json.Stringify.valueAlloc(a, result, .{}) };
}
fn segmentCount(ctx: *anyopaque, a: A, control: t.Control, args: []const V) !V {
    if (args[0] == .null or args[1] == .null) return .null;
    return .{ .int = (try runtime(ctx).segment(a, control, try asFrame(args[0]), try prompt(args[1]))).count };
}
fn segmentScore(ctx: *anyopaque, a: A, control: t.Control, args: []const V) !V {
    if (args[0] == .null or args[1] == .null) return .null;
    return .{ .float = (try runtime(ctx).segment(a, control, try asFrame(args[0]), try prompt(args[1]))).score };
}
fn personCount(ctx: *anyopaque, a: A, control: t.Control, args: []const V) !V {
    return objectCount(ctx, a, control, &.{ args[0], .{ .string = "person" } });
}
fn objectCount(ctx: *anyopaque, a: A, control: t.Control, args: []const V) !V {
    if (args[0] == .null or args[1] == .null) return .null;
    const label = try prompt(args[1]);
    var class: ?usize = null;
    for (classes, 0..) |name, i| {
        if (std.ascii.eqlIgnoreCase(label, name)) {
            class = i;
            break;
        }
    }
    const c = class orelse return error.UnknownObjectClass;
    return .{ .int = (try runtime(ctx).detect(a, control, try asFrame(args[0])))[c] };
}
fn motion(ctx: *anyopaque, a: A, control: t.Control, args: []const V) !V {
    if (args[0] == .null or args[1] == .null) return .null;
    const r = runtime(ctx);
    const path_a = try r.decode(a, control, try asFrame(args[0]), .gray);
    const path_b = try r.decode(a, control, try asFrame(args[1]), .gray);
    const bytes_a = try std.Io.Dir.cwd().readFileAlloc(r.io, path_a, a, .limited(160 * 90 + 1));
    const bytes_b = try std.Io.Dir.cwd().readFileAlloc(r.io, path_b, a, .limited(160 * 90 + 1));
    return .{ .float = try difference(bytes_a, bytes_b) };
}
fn prepareMotion(ctx: *anyopaque, a: A, control: t.Control, rows: []const []const V) !void {
    var frames: std.ArrayList(t.Frame) = .empty;
    for (rows) |args| {
        if (args[0] == .null or args[1] == .null) continue;
        try frames.append(a, try asFrame(args[0]));
        try frames.append(a, try asFrame(args[1]));
    }
    try runtime(ctx).prepareGray(a, control, frames.items);
}
pub fn difference(a: []const u8, b: []const u8) !f64 {
    if (a.len != 160 * 90 or b.len != a.len) return error.InvalidGrayBuffer;
    var sum: u64 = 0;
    for (a, b) |x, y| sum += @abs(@as(i32, x) - @as(i32, y));
    return @as(f64, @floatFromInt(sum)) / (255.0 * 160 * 90);
}
fn validModel(a: A, io: std.Io, path_: []const u8, hash: []const u8) !bool {
    const bytes = std.Io.Dir.cwd().readFileAlloc(io, path_, a, .limited(32 << 20)) catch return false;
    defer a.free(bytes);
    var digest: [32]u8 = undefined;
    std.crypto.hash.sha2.Sha256.hash(bytes, &digest, .{});
    return std.mem.eql(u8, &std.fmt.bytesToHex(digest, .lower), hash);
}
pub const classes = [_][]const u8{
    "person",       "bicycle",   "car",           "motorcycle", "airplane",     "bus",            "train",      "truck",      "boat",          "traffic light",
    "fire hydrant", "stop sign", "parking meter", "bench",      "bird",         "cat",            "dog",        "horse",      "sheep",         "cow",
    "elephant",     "bear",      "zebra",         "giraffe",    "backpack",     "umbrella",       "handbag",    "tie",        "suitcase",      "frisbee",
    "skis",         "snowboard", "sports ball",   "kite",       "baseball bat", "baseball glove", "skateboard", "surfboard",  "tennis racket", "bottle",
    "wine glass",   "cup",       "fork",          "knife",      "spoon",        "bowl",           "banana",     "apple",      "sandwich",      "orange",
    "broccoli",     "carrot",    "hot dog",       "pizza",      "donut",        "cake",           "chair",      "couch",      "potted plant",  "bed",
    "dining table", "toilet",    "tv",            "laptop",     "mouse",        "remote",         "keyboard",   "cell phone", "microwave",     "oven",
    "toaster",      "sink",      "refrigerator",  "book",       "clock",        "vase",           "scissors",   "teddy bear", "hair drier",    "toothbrush",
};
pub fn letterbox(a: A, img: sam3.RgbImage, size: usize) ![]f32 {
    if (img.width == 0 or img.height == 0 or img.width > 1280 or img.height > 1280 or size == 0 or size > 640) return error.InvalidImageDimensions;
    const width: f64 = @floatFromInt(img.width);
    const height: f64 = @floatFromInt(img.height);
    const target: f64 = @floatFromInt(size);
    const scale = @min(target / width, target / height);
    const w: usize = @max(1, @as(usize, @intFromFloat(@round(width * scale))));
    const h: usize = @max(1, @as(usize, @intFromFloat(@round(height * scale))));
    const left = (size - w) / 2;
    const top = (size - h) / 2;
    const out = try a.alloc(f32, 3 * size * size);
    @memset(out, 114.0 / 255.0);
    for (0..h) |y| {
        const sy = @max(0, (@as(f64, @floatFromInt(y)) + 0.5) * height / @as(f64, @floatFromInt(h)) - 0.5);
        const y0: usize = @min(@as(usize, @intFromFloat(sy)), img.height - 1);
        const y1 = @min(y0 + 1, img.height - 1);
        const dy: f32 = @floatCast(sy - @as(f64, @floatFromInt(y0)));
        for (0..w) |x| {
            const sx = @max(0, (@as(f64, @floatFromInt(x)) + 0.5) * width / @as(f64, @floatFromInt(w)) - 0.5);
            const x0: usize = @min(@as(usize, @intFromFloat(sx)), img.width - 1);
            const x1 = @min(x0 + 1, img.width - 1);
            const dx: f32 = @floatCast(sx - @as(f64, @floatFromInt(x0)));
            for (0..3) |c| {
                const aa: f32 = @floatFromInt(img.pixels[(y0 * img.width + x0) * 3 + c]);
                const b: f32 = @floatFromInt(img.pixels[(y0 * img.width + x1) * 3 + c]);
                const cc: f32 = @floatFromInt(img.pixels[(y1 * img.width + x0) * 3 + c]);
                const d: f32 = @floatFromInt(img.pixels[(y1 * img.width + x1) * 3 + c]);
                out[c * size * size + (top + y) * size + left + x] = ((aa + (b - aa) * dx) * (1 - dy) + (cc + (d - cc) * dx) * dy) / 255;
            }
        }
    }
    return out;
}
const DetectionBox = struct { x1: f32, y1: f32, x2: f32, y2: f32, score: f32, class: usize };
pub fn detectionCounts(a: A, data: []const f32, shape: []const i64, threshold: f32) !Counts {
    if (shape.len != 3 or shape[0] != 1 or shape[1] <= 0 or shape[2] <= 0 or shape[1] > 100000 or shape[2] > 100000 or shape[1] * shape[2] != data.len) return error.InvalidDetectionShape;
    var boxes: std.ArrayList(DetectionBox) = .empty;
    defer boxes.deinit(a);
    if (shape[1] == 84) {
        const n: usize = @intCast(shape[2]);
        for (0..n) |i| {
            var class: usize = 0;
            var score = data[4 * n + i];
            for (1..80) |c| {
                if (data[(4 + c) * n + i] > score) {
                    score = data[(4 + c) * n + i];
                    class = c;
                }
            }
            if (score < threshold) continue;
            const x = data[i];
            const y = data[n + i];
            const w = data[2 * n + i];
            const h = data[3 * n + i];
            try boxes.append(a, .{ .x1 = x - w / 2, .y1 = y - h / 2, .x2 = x + w / 2, .y2 = y + h / 2, .score = score, .class = class });
        }
    } else if (shape[2] == 6) {
        var i: usize = 0;
        while (i < data.len) : (i += 6) {
            if (data[i + 4] < threshold) continue;
            const c = data[i + 5];
            if (!std.math.isFinite(c) or c < 0 or c >= 80 or c != @trunc(c)) return error.InvalidDetectionClass;
            try boxes.append(a, .{ .x1 = data[i], .y1 = data[i + 1], .x2 = data[i + 2], .y2 = data[i + 3], .score = data[i + 4], .class = @intFromFloat(c) });
        }
    } else return error.InvalidDetectionShape;
    for (boxes.items) |b| {
        if (!std.math.isFinite(b.x1) or !std.math.isFinite(b.y1) or !std.math.isFinite(b.x2) or !std.math.isFinite(b.y2) or !std.math.isFinite(b.score) or b.score > 1 or b.x2 <= b.x1 or b.y2 <= b.y1) return error.InvalidDetectionValue;
    }
    const Sort = struct {
        fn less(_: void, x: DetectionBox, y: DetectionBox) bool {
            return x.score > y.score;
        }
    };
    std.mem.sort(DetectionBox, boxes.items, {}, Sort.less);
    var selected: [300]DetectionBox = undefined;
    var n: usize = 0;
    var result: Counts = @splat(0);
    outer: for (boxes.items) |b| {
        for (selected[0..n]) |earlier| {
            if (earlier.class == b.class and iou(earlier, b) > 0.7) continue :outer;
        }
        selected[n] = b;
        n += 1;
        result[b.class] += 1;
        if (n == 300) break;
    }
    return result;
}
fn iou(a: DetectionBox, b: DetectionBox) f32 {
    const area = @max(0, @min(a.x2, b.x2) - @max(a.x1, b.x1)) * @max(0, @min(a.y2, b.y2) - @max(a.y1, b.y1));
    const union_area = (a.x2 - a.x1) * (a.y2 - a.y1) + (b.x2 - b.x1) * (b.y2 - b.y1) - area;
    return if (union_area <= 0) 0 else area / union_area;
}
test "motion extremes and class-aware detection suppression" {
    const a = std.testing.allocator;
    const black: [14400]u8 = @splat(0);
    const white: [14400]u8 = @splat(255);
    try std.testing.expectEqual(@as(f64, 1), try difference(&black, &white));
    const counts = try detectionCounts(a, &.{ 1, 1, 10, 10, 0.9, 0, 1, 1, 10, 10, 0.8, 0, 1, 1, 10, 10, 0.9, 2 }, &.{ 1, 3, 6 }, 0.25);
    try std.testing.expectEqual(@as(u32, 1), counts[0]);
    try std.testing.expectEqual(@as(u32, 1), counts[2]);
}
