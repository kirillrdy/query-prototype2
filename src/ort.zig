//! Small in-process binding to the stable ONNX Runtime C API. Slots below are
//! from the official 1.23.2 onnxruntime_c_api.h / OrtApi (API version 23).
//! SAM3 uses its own native Zig runtime; YOLO retains its CPU runtime so the
//! migration preserves existing model compatibility and counting behavior.
const std = @import("std");
const builtin = @import("builtin");
const A = std.mem.Allocator;
const P = ?*anyopaque;
const Api = [*]const *const anyopaque;
const Base = extern struct { get_api: *const fn (u32) callconv(.c) ?Api, version: *const fn () callconv(.c) [*:0]const u8 };
pub const Output = struct { data: []const f32, shape: []const i64 };
pub const Detector = struct {
    library: *anyopaque,
    api: Api,
    env: P = null,
    session: P = null,
    memory: P = null,
    allocator: P = null,
    input: ?[*:0]u8 = null,
    output: ?[*:0]u8 = null,
    fn fun(d: Detector, comptime T: type, slot: usize) T {
        return @ptrCast(d.api[slot]);
    }
    fn check(d: Detector, status: P) !void {
        if (status) |s| {
            const message = d.fun(*const fn (P) callconv(.c) [*:0]const u8, 2)(s);
            std.log.err("ONNX Runtime: {s}", .{std.mem.span(message)});
            d.fun(*const fn (P) callconv(.c) void, 93)(s);
            return error.ONNXRuntime;
        }
    }
    pub fn open(a: A, io: std.Io, model: []const u8, library: []const u8) !Detector {
        const path = try a.dupeSentinel(u8, library, 0);
        defer a.free(path);
        var handle = std.c.dlopen(path, .{ .NOW = true, .GLOBAL = true });
        if (handle == null) {
            preload(a, io);
            handle = std.c.dlopen(path, .{ .NOW = true, .GLOBAL = true });
        }
        const lib = handle orelse return error.ONNXRuntimeLibraryNotFound;
        errdefer _ = std.c.dlclose(lib);
        const entry: *const fn () callconv(.c) *const Base = @ptrCast(std.c.dlsym(lib, "OrtGetApiBase") orelse return error.ONNXRuntimeMissingAPI);
        var d = Detector{ .library = lib, .api = entry().get_api(23) orelse return error.ONNXRuntimeAPIVersion };
        errdefer d.release();
        try d.check(d.fun(*const fn (c_int, [*:0]const u8, *P) callconv(.c) P, 3)(3, "binaryquery", &d.env));
        try d.check(d.fun(*const fn (*P) callconv(.c) P, 78)(&d.allocator));
        try d.check(d.fun(*const fn (c_int, c_int, *P) callconv(.c) P, 69)(1, 0, &d.memory));
        var options: P = null;
        try d.check(d.fun(*const fn (*P) callconv(.c) P, 10)(&options));
        defer d.fun(*const fn (P) callconv(.c) void, 100)(options);
        try d.check(d.fun(*const fn (P, c_int) callconv(.c) P, 24)(options, 4));
        const model_z = try a.dupeSentinel(u8, model, 0);
        defer a.free(model_z);
        try d.check(d.fun(*const fn (P, [*:0]const u8, P, *P) callconv(.c) P, 7)(d.env, model_z, options, &d.session));
        var count: usize = 0;
        try d.check(d.fun(*const fn (P, *usize) callconv(.c) P, 30)(d.session, &count));
        if (count != 1) return error.InvalidDetectionModel;
        try d.check(d.fun(*const fn (P, *usize) callconv(.c) P, 31)(d.session, &count));
        if (count != 1) return error.InvalidDetectionModel;
        try d.check(d.fun(*const fn (P, usize, P, *?[*:0]u8) callconv(.c) P, 36)(d.session, 0, d.allocator, &d.input));
        try d.check(d.fun(*const fn (P, usize, P, *?[*:0]u8) callconv(.c) P, 37)(d.session, 0, d.allocator, &d.output));
        return d;
    }
    fn release(d: *Detector) void {
        if (d.input) |name| {
            const status = d.fun(*const fn (P, P) callconv(.c) P, 76)(d.allocator, name);
            d.check(status) catch {};
        }
        if (d.output) |name| {
            const status = d.fun(*const fn (P, P) callconv(.c) P, 76)(d.allocator, name);
            d.check(status) catch {};
        }
        if (d.session) |s| d.fun(*const fn (P) callconv(.c) void, 95)(s);
        if (d.memory) |m| d.fun(*const fn (P) callconv(.c) void, 94)(m);
        if (d.env) |e| d.fun(*const fn (P) callconv(.c) void, 92)(e);
    }
    pub fn deinit(d: *Detector) void {
        d.release();
        _ = std.c.dlclose(d.library);
    }
    pub fn run(d: Detector, a: A, input: []f32) !Output {
        var tensor: P = null;
        const shape = [_]i64{ 1, 3, 640, 640 };
        try d.check(d.fun(*const fn (P, P, usize, [*]const i64, usize, c_int, *P) callconv(.c) P, 49)(d.memory, input.ptr, input.len * 4, &shape, shape.len, 1, &tensor));
        defer d.fun(*const fn (P) callconv(.c) void, 96)(tensor);
        const in_names = [_][*:0]const u8{d.input.?};
        const out_names = [_][*:0]const u8{d.output.?};
        const values = [_]P{tensor};
        var output: P = null;
        try d.check(d.fun(*const fn (P, P, [*]const [*:0]const u8, [*]const P, usize, [*]const [*:0]const u8, usize, [*]P) callconv(.c) P, 9)(d.session, null, &in_names, &values, 1, &out_names, 1, @ptrCast(&output)));
        defer d.fun(*const fn (P) callconv(.c) void, 96)(output);
        var info: P = null;
        try d.check(d.fun(*const fn (P, *P) callconv(.c) P, 65)(output, &info));
        defer d.fun(*const fn (P) callconv(.c) void, 99)(info);
        var element: c_int = 0;
        try d.check(d.fun(*const fn (P, *c_int) callconv(.c) P, 60)(info, &element));
        if (element != 1) return error.ExpectedFloatTensor;
        var dimensions: usize = 0;
        try d.check(d.fun(*const fn (P, *usize) callconv(.c) P, 61)(info, &dimensions));
        if (dimensions != 3) return error.InvalidDetectionShape;
        const dims = try a.alloc(i64, dimensions);
        errdefer a.free(dims);
        try d.check(d.fun(*const fn (P, [*]i64, usize) callconv(.c) P, 62)(info, dims.ptr, dims.len));
        var count: usize = 0;
        try d.check(d.fun(*const fn (P, *usize) callconv(.c) P, 64)(info, &count));
        if (count > 10_000_000) return error.InvalidDetectionShape;
        var data: P = null;
        try d.check(d.fun(*const fn (P, *P) callconv(.c) P, 51)(output, &data));
        const floats: [*]const f32 = @ptrCast(@alignCast(data.?));
        return .{ .data = try a.dupe(f32, floats[0..count]), .shape = dims };
    }
};
fn preload(a: A, io: std.Io) void {
    if (builtin.os.tag != .linux) return;
    for ([_][:0]const u8{ "libgcc_s.so.1", "libstdc++.so.6" }) |name| {
        if (std.c.dlopen(name, .{ .NOW = true, .GLOBAL = true }) != null) continue;
        var dir = std.Io.Dir.cwd().openDir(io, "/nix/store", .{ .iterate = true }) catch continue;
        defer dir.close(io);
        var it = dir.iterate();
        while (it.next(io) catch null) |entry| {
            if (std.mem.indexOf(u8, entry.name, "-gcc-") == null or !std.mem.endsWith(u8, entry.name, "-lib") or std.mem.indexOf(u8, entry.name, "-unknown-") != null) continue;
            const path = std.fmt.allocPrintSentinel(a, "/nix/store/{s}/lib/{s}", .{ entry.name, name }, 0) catch continue;
            defer a.free(path);
            if (std.c.dlopen(path, .{ .NOW = true, .GLOBAL = true }) != null) break;
        }
    }
}
