const std = @import("std");
const builtin = @import("builtin");
extern "c" fn signal(c_int, ?*const fn (c_int) callconv(.c) void) ?*const fn (c_int) callconv(.c) void;
extern "c" fn poll([*]PollFd, usize, c_int) c_int;
extern "c" fn recv(c_int, [*]u8, usize, c_int) isize;
extern "c" fn setsockopt(c_int, c_int, c_int, *const anyopaque, u32) c_int;
pub const PollFd = extern struct { fd: c_int, events: c_short, revents: c_short = 0 };
var interrupted = std.atomic.Value(bool).init(false);
fn stop(_: c_int) callconv(.c) void {
    interrupted.store(true, .release);
}
pub fn install() void {
    _ = signal(2, stop);
    _ = signal(15, stop);
    _ = signal(13, @ptrFromInt(1));
}
pub fn stopped() bool {
    return interrupted.load(.acquire);
}
pub fn ready(fd: c_int) bool {
    var f = PollFd{ .fd = fd, .events = 1 };
    return poll(@ptrCast(&f), 1, 0) > 0;
}
pub const RequestControl = struct {
    fd: ?c_int = null,
    deadline_ns: ?i96 = null,
    io: std.Io,
    pub fn canceled(ctx: *anyopaque) bool {
        const c: *RequestControl = @ptrCast(@alignCast(ctx));
        if (stopped()) return true;
        if (c.deadline_ns) |deadline| {
            if (std.Io.Timestamp.now(c.io, .awake).nanoseconds >= deadline) return true;
        }
        if (c.fd) |fd| {
            if (ready(fd)) {
                var b: [1]u8 = undefined;
                if (recv(fd, &b, 1, 2) <= 0) return true;
            }
        }
        return false;
    }
};
pub fn timeout(fd: c_int) void {
    const Timeval = extern struct { sec: c_long, usec: c_long };
    const value = Timeval{ .sec = 15, .usec = 0 };
    const darwin = builtin.os.tag.isDarwin();
    _ = setsockopt(fd, if (darwin) 0xffff else 1, if (darwin) 0x1006 else 20, &value, @sizeOf(Timeval));
    _ = setsockopt(fd, if (darwin) 0xffff else 1, if (darwin) 0x1005 else 21, &value, @sizeOf(Timeval));
}
