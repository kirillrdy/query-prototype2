const std = @import("std");

pub fn build(b: *std.Build) void {
    const target = b.standardTargetOptions(.{});
    const optimize = b.standardOptimizeOption(.{});
    const backend = b.option([]const u8, "backend", "Inference backend: opencl, cuda, metal") orelse (if (target.result.os.tag.isDarwin()) "metal" else "opencl");
    const sm = b.option([]const u8, "sm", "CUDA compute capability") orelse "sm_89";
    const native = b.dependency("onnx", .{
        .target = target,
        .optimize = optimize,
        .backend = backend,
        .sm = sm,
    });
    const dep = b.dependency("sam3", .{
        .target = target,
        .optimize = optimize,
        .backend = backend,
        .sm = sm,
    });
    // Share one runtime module, including the YOLO Softmax compatibility fix.
    dep.module("sam3").addImport("onnx", native.module("onnx"));
    const core = b.addModule("query", .{
        .root_source_file = b.path("src/root.zig"),
        .target = target,
        .optimize = optimize,
        .link_libc = true,
        .imports = &.{
            .{ .name = "sam3", .module = dep.module("sam3") },
            .{ .name = "onnx", .module = native.module("onnx") },
        },
    });
    inline for (.{ .{ "bquery", "src/main.zig", "run" }, .{ "bquery-web", "src/web_main.zig", "web" } }) |entry| {
        const exe = b.addExecutable(.{ .name = entry[0], .root_module = b.createModule(.{
            .root_source_file = b.path(entry[1]),
            .target = target,
            .optimize = optimize,
            .link_libc = true,
            .imports = &.{.{ .name = "query", .module = core }},
        }) });
        b.installArtifact(exe);
        const run = b.addRunArtifact(exe);
        run.addPassthruArgs();
        b.step(entry[2], "Run " ++ entry[0]).dependOn(&run.step);
    }
    const tests = b.addTest(.{ .root_module = core });
    const test_run = b.addRunArtifact(tests);
    // Optional integration fixtures come from the environment and can change
    // without recompilation. Always execute tests rather than caching results.
    test_run.has_side_effects = true;
    const test_step = b.step("test", "Run native query and ONNX runtime tests");
    test_step.dependOn(&test_run.step);
    const runtime_tests = b.addRunArtifact(b.addTest(.{ .root_module = native.module("onnx") }));
    test_step.dependOn(&runtime_tests.step);
}
