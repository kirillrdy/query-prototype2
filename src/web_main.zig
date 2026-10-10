const std = @import("std");
const q = @import("query");
pub fn main(init: std.process.Init) void {
    q.web.run(init) catch |err| {
        std.debug.print("bquery-web: {s}\n", .{@errorName(err)});
        std.process.exit(1);
    };
}
