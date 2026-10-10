const std = @import("std");
const builtin = @import("builtin");
const t = @import("types.zig");
pub fn runtimePath(r: anytype, a: std.mem.Allocator, c: t.Control) ![]const u8 {
    const target = switch (builtin.os.tag) {
        .linux => switch (builtin.cpu.arch) {
            .x86_64 => .{ "linux-x64", "1fa4dcaef22f6f7d5cd81b28c2800414350c10116f5fdd46a2160082551c5f9b", "libonnxruntime.so.1.23.2" },
            .aarch64 => .{ "linux-aarch64", "7c63c73560ed76b1fac6cff8204ffe34fe180e70d6582b5332ec094810241e5c", "libonnxruntime.so.1.23.2" },
            else => return error.UnsupportedDetectionPlatform,
        },
        .macos => switch (builtin.cpu.arch) {
            .x86_64 => .{ "osx-x86_64", "d10359e16347b57d9959f7e80a225a5b4a66ed7d7e007274a15cae86836485a6", "libonnxruntime.1.23.2.dylib" },
            .aarch64 => .{ "osx-arm64", "b4d513ab2b26f088c66891dbbc1408166708773d7cc4163de7bdca0e9bbb7856", "libonnxruntime.1.23.2.dylib" },
            else => return error.UnsupportedDetectionPlatform,
        },
        else => return error.UnsupportedDetectionPlatform,
    };
    const cache = t.env("BQUERY_DETECTION_CACHE") orelse try std.fmt.allocPrint(a, "{s}/binaryquery/detection", .{t.env("XDG_CACHE_HOME") orelse try std.fmt.allocPrint(a, "{s}/.cache", .{t.env("HOME") orelse return error.HomeNotSet})});
    try std.Io.Dir.cwd().createDirPath(r.io, cache);
    const name = try std.fmt.allocPrint(a, "onnxruntime-{s}-1.23.2", .{target[0]});
    const archive = try std.fmt.allocPrint(a, "{s}/{s}.tgz", .{ cache, name });
    if (!valid(a, r.io, archive, target[1])) {
        const staging = try std.fmt.allocPrint(a, "{s}.part", .{archive});
        defer std.Io.Dir.cwd().deleteFile(r.io, staging) catch {};
        _ = try r.command(a, c, &.{ "curl", "-fsSL", "--retry", "3", "--max-filesize", "134217728", "-o", staging, try std.fmt.allocPrint(a, "https://github.com/microsoft/onnxruntime/releases/download/v1.23.2/{s}.tgz", .{name}) }, 1 << 20);
        if (!valid(a, r.io, staging, target[1])) return error.RuntimeChecksumMismatch;
        try std.Io.Dir.cwd().rename(staging, .cwd(), archive, r.io);
    }
    const bytes = try r.command(a, c, &.{ "tar", "-xOzf", archive, try std.fmt.allocPrint(a, "{s}/lib/{s}", .{ name, target[2] }) }, 128 << 20);
    defer a.free(bytes);
    if (bytes.len == 0) return error.InvalidRuntimeLibrary;
    var digest: [32]u8 = undefined;
    std.crypto.hash.sha2.Sha256.hash(bytes, &digest, .{});
    const hash = std.fmt.bytesToHex(digest, .lower);
    const path = try std.fmt.allocPrint(a, "{s}/{s}-{s}", .{ cache, hash[0..12], target[2] });
    if (!valid(a, r.io, path, &hash)) {
        const staging = try std.fmt.allocPrint(a, "{s}/library.part", .{r.temp});
        defer std.Io.Dir.cwd().deleteFile(r.io, staging) catch {};
        const file = try std.Io.Dir.cwd().createFile(r.io, staging, .{});
        defer file.close(r.io);
        try file.writeStreamingAll(r.io, bytes);
        try std.Io.Dir.cwd().rename(staging, .cwd(), path, r.io);
    }
    return path;
}
fn valid(a: std.mem.Allocator, io: std.Io, path: []const u8, hash: []const u8) bool {
    const bytes = std.Io.Dir.cwd().readFileAlloc(io, path, a, .limited(128 << 20)) catch return false;
    defer a.free(bytes);
    var digest: [32]u8 = undefined;
    std.crypto.hash.sha2.Sha256.hash(bytes, &digest, .{});
    return std.mem.eql(u8, &std.fmt.bytesToHex(digest, .lower), hash);
}
