const std = @import("std");

pub fn build(b: *std.Build) void {
    const target = b.standardTargetOptions(.{});
    const optimize = b.standardOptimizeOption(.{});
    const backend = b.option([]const u8, "backend", "SAM3 backend: opencl, cuda, metal") orelse
        (if (target.result.os.tag.isDarwin()) "metal" else "opencl");
    const dep = b.dependency("sam3", .{
        .target = target,
        .optimize = optimize,
        .backend = backend,
        .sm = b.option([]const u8, "sm", "CUDA compute capability") orelse "sm_89",
    });
    const exe = b.addExecutable(.{
        .name = "bquery-sam3",
        .root_module = b.createModule(.{
            .root_source_file = b.path("main.zig"),
            .target = target,
            .optimize = optimize,
            .link_libc = true,
            .imports = &.{.{ .name = "sam3", .module = dep.module("sam3") }},
        }),
    });
    b.installArtifact(exe);
}
