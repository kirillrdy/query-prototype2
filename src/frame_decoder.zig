//! A forward frame iterator. FFprobe and FFmpeg stay alive for the scan;
//! packet positions associate decoded pictures with exact MP4 samples.
const std = @import("std");
const t = @import("types.zig");
const control = @import("control.zig");
const A = std.mem.Allocator;
extern "c" fn kill(c_int, c_int) c_int;

fn stopChild(child: *std.process.Child, io: std.Io) void {
    // FFmpeg can block in shutdown while its output pipe is backpressured.
    // These scan-owned processes have no output to preserve after close.
    if (child.stdout) |file| {
        file.close(io);
        child.stdout = null;
    }
    if (child.id) |pid| _ = kill(pid, 9);
    child.kill(io);
}

const Pipe = struct {
    file: std.Io.File,
    buffer: [8192]u8 = undefined,
    start: usize = 0,
    end: usize = 0,

    fn byte(p: *Pipe, c: t.Control) !u8 {
        if (p.start == p.end) {
            while (!control.ready(p.file.handle)) {
                try c.check();
                try std.Io.sleep(c.io, .fromMilliseconds(1), .awake);
            }
            try c.check();
            p.end = try p.file.readStreaming(c.io, &.{&p.buffer});
            p.start = 0;
            if (p.end == 0) return error.EndOfStream;
        }
        const value = p.buffer[p.start];
        p.start += 1;
        return value;
    }

    fn position(p: *Pipe, c: t.Control) !u64 {
        var line: [256]u8 = undefined;
        while (true) {
            var n: usize = 0;
            while (true) {
                const b = try p.byte(c);
                if (b == '\n') break;
                if (n == line.len) return error.InvalidFramePosition;
                line[n] = b;
                n += 1;
            }
            const text = std.mem.trim(u8, line[0..n], " \r\t");
            if (text.len == 0) continue;
            return std.fmt.parseInt(u64, text, 10) catch error.InvalidFramePosition;
        }
    }
};

pub const Decoder = struct {
    a: A,
    io: std.Io,
    source: []const u8,
    track: u32,
    last_frame: usize = 0,
    last_position: ?u64 = null,
    probe: std.process.Child,
    decoder: std.process.Child,
    positions: Pipe,
    pictures: Pipe,
    jpeg: std.ArrayList(u8) = .empty,

    pub fn open(a: A, c: t.Control, first: t.Frame) !Decoder {
        try c.check();
        var arena = std.heap.ArenaAllocator.init(a);
        defer arena.deinit();
        const scratch = arena.allocator();
        const source = try a.dupe(u8, first.source);
        errdefer a.free(source);
        const start = if (first.seekable) @max(0, first.decode_start - 0.000001) else 0;
        var probe_args: std.ArrayList([]const u8) = .empty;
        try probe_args.appendSlice(scratch, &.{ "ffprobe", "-v", "error", "-ignore_editlist", "1" });
        if (start > 0) try probe_args.appendSlice(scratch, &.{ "-read_intervals", try std.fmt.allocPrint(scratch, "{d:.6}%", .{start}) });
        try probe_args.appendSlice(scratch, &.{ "-select_streams", try std.fmt.allocPrint(scratch, "i:{d}", .{first.track_id}), "-show_frames", "-show_entries", "frame=pkt_pos:frame_side_data=", "-of", "default=noprint_wrappers=1:nokey=1", source });
        var probe = try std.process.spawn(c.io, .{ .argv = probe_args.items, .stdin = .ignore, .stdout = .pipe, .stderr = .inherit });
        errdefer stopChild(&probe, c.io);
        var args: std.ArrayList([]const u8) = .empty;
        try args.appendSlice(scratch, &.{ "ffmpeg", "-v", "error", "-nostdin", "-ignore_editlist", "1" });
        if (start > 0) try args.appendSlice(scratch, &.{ "-ss", try std.fmt.allocPrint(scratch, "{d:.6}", .{start}), "-seek_timestamp", "1", "-noaccurate_seek" });
        // Keep every decoded picture, including equal presentation timestamps.
        try args.appendSlice(scratch, &.{ "-i", source, "-map", try std.fmt.allocPrint(scratch, "0:i:{d}", .{first.track_id}), "-an", "-vf", "scale=w='min(1280,iw)':h='min(720,ih)':force_original_aspect_ratio=decrease", "-fps_mode", "passthrough", "-q:v", "2", "-c:v", "mjpeg", "-f", "image2pipe", "pipe:1" });
        const decoder = try std.process.spawn(c.io, .{ .argv = args.items, .stdin = .ignore, .stdout = .pipe, .stderr = .inherit });
        return .{ .a = a, .io = c.io, .source = source, .track = first.track_id, .probe = probe, .decoder = decoder, .positions = .{ .file = probe.stdout.? }, .pictures = .{ .file = decoder.stdout.? } };
    }

    pub fn deinit(d: *Decoder) void {
        stopChild(&d.probe, d.io);
        stopChild(&d.decoder, d.io);
        d.jpeg.deinit(d.a);
        d.a.free(d.source);
    }

    pub fn accepts(d: Decoder, frame: t.Frame) bool {
        return std.mem.eql(u8, d.source, frame.source) and d.track == frame.track_id and frame.frame_index >= d.last_frame;
    }

    /// The returned JPEG is borrowed until the next read or deinit.
    pub fn read(d: *Decoder, c: t.Control, frame: t.Frame) ![]const u8 {
        if (!d.accepts(frame)) return error.NonSequentialFrame;
        try c.check();
        if (d.last_position != null and d.last_position.? == frame.offset) return d.jpeg.items;
        while (true) {
            try c.check();
            const position = d.positions.position(c) catch |err| switch (err) {
                error.EndOfStream => return error.FramePositionNotFound,
                else => return err,
            };
            try readJpeg(d.a, &d.jpeg, &d.pictures, c);
            d.last_position = position;
            if (position == frame.offset) {
                d.last_frame = frame.frame_index;
                return d.jpeg.items;
            }
        }
    }
};

fn take(a: A, out: *std.ArrayList(u8), reader: anytype, c: t.Control) !u8 {
    if (out.items.len >= 32 << 20) return error.ImageTooLarge;
    const b = try reader.byte(c);
    try out.append(a, b);
    return b;
}

fn marker(a: A, out: *std.ArrayList(u8), reader: anytype, c: t.Control) !u8 {
    if (try take(a, out, reader, c) != 0xff) return error.InvalidJpegStream;
    var value = try take(a, out, reader, c);
    while (value == 0xff) value = try take(a, out, reader, c);
    return value;
}

// Honor segment lengths: FF D9 inside metadata is not an end-of-image marker.
fn readJpeg(a: A, out: *std.ArrayList(u8), reader: anytype, c: t.Control) !void {
    out.clearRetainingCapacity();
    if (try marker(a, out, reader, c) != 0xd8) return error.InvalidJpegStream;
    var kind = try marker(a, out, reader, c);
    while (kind != 0xd9) {
        if (kind == 0 or kind == 0xd8 or (kind >= 0xd0 and kind <= 0xd7)) return error.InvalidJpegStream;
        if (kind != 1) {
            const hi = try take(a, out, reader, c);
            const lo = try take(a, out, reader, c);
            const length = @as(usize, hi) * 256 + lo;
            if (length < 2) return error.InvalidJpegStream;
            for (0..length - 2) |_| _ = try take(a, out, reader, c);
        }
        if (kind == 0xda) {
            while (true) {
                if (try take(a, out, reader, c) != 0xff) continue;
                kind = try take(a, out, reader, c);
                while (kind == 0xff) kind = try take(a, out, reader, c);
                if (kind == 0 or (kind >= 0xd0 and kind <= 0xd7)) continue;
                break;
            }
        } else kind = try marker(a, out, reader, c);
    }
}

test "JPEG iterator respects metadata lengths, escaped bytes, and image boundaries" {
    const Reader = struct {
        bytes: []const u8,
        fn byte(r: *@This(), _: t.Control) !u8 {
            if (r.bytes.len == 0) return error.EndOfStream;
            const b = r.bytes[0];
            r.bytes = r.bytes[1..];
            return b;
        }
    };
    const jpeg = "\xff\xd8\xff\xe0\x00\x04\xff\xd9\xff\xda\x00\x02\x11\xff\x00\xd9\xff\xd0\x22\xff\xd9";
    var reader = Reader{ .bytes = jpeg ++ jpeg };
    var out: std.ArrayList(u8) = .empty;
    defer out.deinit(std.testing.allocator);
    for (0..2) |_| {
        try readJpeg(std.testing.allocator, &out, &reader, .{ .io = std.testing.io });
        try std.testing.expectEqualSlices(u8, jpeg, out.items);
    }
    try std.testing.expectEqual(@as(usize, 0), reader.bytes.len);
}
