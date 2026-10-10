const std = @import("std");
const sam3 = @import("sam3");

extern "c" fn getchar() c_int;
extern "c" fn write(c_int, [*]const u8, usize) isize;

const Request = struct { path: []const u8, phrase: []const u8, min_score: f32 };
const Mask = struct { score: f32, rle: []const u32 };
const Result = struct { count: usize, score: f32, width: usize, height: usize, masks: []const Mask };

const Cache = struct {
    path: ?[]const u8 = null,
    embedding: ?sam3.TextImageEmbedding = null,
    phrase: ?[]const u8 = null,
    features: ?[]f32 = null,

    fn deinit(self: *Cache, a: std.mem.Allocator) void {
        if (self.embedding) |*e| e.deinit();
        if (self.path) |p| a.free(p);
        if (self.phrase) |p| a.free(p);
        if (self.features) |f| a.free(f);
    }
};

fn send(allocator: std.mem.Allocator, value: anytype) !void {
    const json = try std.json.Stringify.valueAlloc(allocator, value, .{});
    defer allocator.free(json);
    var offset: usize = 0;
    while (offset < json.len) {
        const n = write(1, json.ptr + offset, json.len - offset);
        if (n <= 0) return error.OutputClosed;
        offset += @intCast(n);
    }
    if (write(1, "\n", 1) != 1) return error.OutputClosed;
}

pub fn main(init: std.process.Init) !void {
    const allocator = init.gpa;
    var model: ?sam3.Model = null;
    defer if (model) |*m| m.deinit();
    var cache: Cache = .{};
    defer cache.deinit(allocator);
    var line: std.Io.Writer.Allocating = .init(allocator);
    defer line.deinit();
    while (true) {
        line.clearRetainingCapacity();
        while (true) {
            const ch = getchar();
            if (ch < 0) return;
            if (ch == '\n') break;
            if (line.written().len >= 65536) return error.RequestTooLarge;
            try line.writer.writeByte(@intCast(ch));
        }
        var arena = std.heap.ArenaAllocator.init(allocator);
        defer arena.deinit();
        const a = arena.allocator();
        const parsed = std.json.parseFromSlice(Request, a, line.written(), .{}) catch |err| {
            try send(a, .{ .err = @errorName(err) });
            continue;
        };
        const result = infer(a, init.io, allocator, &model, &cache, parsed.value) catch |err| {
            const detail = sam3.onnx.lastError();
            try send(a, .{ .err = try std.fmt.allocPrint(a, "{s}: {s}", .{ @errorName(err), detail }) });
            continue;
        };
        try send(a, result);
    }
}

fn infer(a: std.mem.Allocator, io: std.Io, model_allocator: std.mem.Allocator, model: *?sam3.Model, cache: *Cache, request: Request) !Result {
    if (request.phrase.len == 0 or request.phrase.len > 4096 or
        !std.math.isFinite(request.min_score) or request.min_score <= 0 or request.min_score > 1)
        return error.InvalidRequest;
    if (model.* == null) model.* = try sam3.Model.open(model_allocator, io);
    if (cache.path == null or !std.mem.eql(u8, cache.path.?, request.path)) {
        if (cache.embedding) |*e| e.deinit();
        cache.embedding = null;
        if (cache.path) |p| model_allocator.free(p);
        cache.path = null;
        const bytes = try std.Io.Dir.cwd().readFileAlloc(io, request.path, a, .limited(32 * 1024 * 1024));
        var image = try sam3.decodeImage(a, bytes);
        defer image.deinit(a);
        const path = try model_allocator.dupe(u8, request.path);
        errdefer model_allocator.free(path);
        cache.embedding = try model.*.?.encodeForText(sam3.RgbImage.fromImage(image));
        cache.path = path;
    }
    if (cache.phrase == null or !std.mem.eql(u8, cache.phrase.?, request.phrase)) {
        if (cache.features) |f| model_allocator.free(f);
        cache.features = null;
        if (cache.phrase) |p| model_allocator.free(p);
        cache.phrase = null;
        const phrase = try model_allocator.dupe(u8, request.phrase);
        errdefer model_allocator.free(phrase);
        cache.features = try model.*.?.encodeTextFeatures(request.phrase);
        cache.phrase = phrase;
    }
    var masks = try model.*.?.findWithTextFeatures(&cache.embedding.?, request.phrase, cache.features.?, .{ .min_score = request.min_score });
    defer masks.deinit();
    const result = try a.alloc(Mask, masks.count);
    for (result, 0..) |*mask, i| {
        var runs: std.ArrayList(u32) = .empty;
        var foreground = false;
        var length: u32 = 0;
        for (masks.plane(i)) |logit| {
            const next = logit > 0;
            if (next != foreground) {
                try runs.append(a, length);
                length = 0;
                foreground = next;
            }
            length += 1;
        }
        try runs.append(a, length);
        mask.* = .{ .score = masks.scores[i], .rle = try runs.toOwnedSlice(a) };
    }
    return .{ .count = masks.count, .score = masks.object_score, .width = masks.width, .height = masks.height, .masks = result };
}
