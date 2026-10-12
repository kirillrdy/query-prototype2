//! YOLO inference through the same native Zig ONNX runtime as SAM3.
const std = @import("std");
const onnx = @import("onnx");
const A = std.mem.Allocator;

pub const Output = struct { data: []const f32, shape: []const i64 };

pub const Detector = struct {
    allocator: A,
    env: onnx.Env,
    session: onnx.Session,
    input: [:0]const u8,
    output: [:0]const u8,

    pub fn open(a: A, io: std.Io, model: []const u8) !Detector {
        const env = try onnx.Env.init(a, io);
        errdefer env.deinit();
        const session = try onnx.Session.open(env, model);
        errdefer session.deinit();
        const graph = session.state.graph;
        if (graph.inputs.len != 1 or graph.outputs.len != 1) return error.InvalidDetectionModel;
        const info = graph.inputs[0];
        const shape = [_]i64{ 1, 3, 640, 640 };
        if (info.dtype != .f32) return error.ExpectedFloatTensor;
        if (info.dims.len != shape.len) return error.InvalidDetectionShape;
        for (info.dims, shape) |dim, expected| {
            if (dim > 0 and dim != expected) return error.InvalidDetectionShape;
        }
        const input = try a.dupeSentinel(u8, info.name, 0);
        errdefer a.free(input);
        const output = try a.dupeSentinel(u8, graph.outputs[0].name, 0);
        return .{ .allocator = a, .env = env, .session = session, .input = input, .output = output };
    }

    pub fn deinit(d: *Detector) void {
        d.allocator.free(d.output);
        d.allocator.free(d.input);
        d.session.deinit();
        d.env.deinit();
    }

    pub fn run(d: Detector, a: A, input: []const f32) !Output {
        if (input.len != 3 * 640 * 640) return error.InvalidDetectionShape;
        const shape = [_]i64{ 1, 3, 640, 640 };
        const value = try onnx.Value.borrowF32(input, &shape);
        var outputs: [1]onnx.Value = undefined;
        d.session.run(&.{d.input.ptr}, &.{value}, &.{d.output.ptr}, &outputs) catch |err| {
            std.log.err("YOLO inference: {s}", .{onnx.lastError()});
            return err;
        };
        defer outputs[0].deinit();
        if (outputs[0].dtype != .f32) return error.ExpectedFloatTensor;
        if (outputs[0].dims.len != 3) return error.InvalidDetectionShape;
        const data = try outputs[0].dataF32();
        if (data.len > 10_000_000) return error.InvalidDetectionShape;
        const dims = try a.dupe(i64, outputs[0].dims);
        errdefer a.free(dims);
        return .{ .data = try a.dupe(f32, data), .shape = dims };
    }
};
