const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const net = require('node:net');
const { spawn } = require('node:child_process');

test('full-video index progress reaches the client before inference fills the body buffer', { skip: !process.env.BQUERY_TEST_VIDEO, timeout: 15000 }, async () => {
  const cache = fs.mkdtempSync(path.join(os.tmpdir(), 'bquery-stream-progress-'));
  const listener = net.createServer();
  await new Promise(resolve => listener.listen(0, '127.0.0.1', resolve));
  const port = listener.address().port;
  await new Promise(resolve => listener.close(resolve));
  const binary = process.env.BQUERY_TEST_WEB_BIN || path.resolve(__dirname, '../zig-out/bin/bquery-web');
  const server = spawn(binary, ['-port', String(port)], { env: { ...process.env, BQUERY_INDEX_CACHE: cache }, stdio: ['ignore', 'ignore', 'pipe'] });
  server.stderr.resume();
  const controller = new AbortController();
  try {
    const url = 'http://127.0.0.1:' + port;
    let ready = false;
    for (let n = 0; n < 100; n++) {
      try { ready = (await fetch(url + '/api/info', { signal: AbortSignal.timeout(1000) })).ok; } catch {}
      if (ready) break;
      await new Promise(resolve => setTimeout(resolve, 20));
    }
    assert(ready, 'Server did not start');
    const filename = process.env.BQUERY_TEST_VIDEO.replaceAll("'", "''");
    const response = await fetch(url + '/api/query/stream', {
      method: 'POST', headers: { 'content-type': 'application/json', origin: url },
      body: JSON.stringify({ sql: `CREATE INDEX startup_test ON frames('${filename}') (person_count(frame))` }),
      signal: AbortSignal.any([controller.signal, AbortSignal.timeout(5000)])
    });
    assert(response.ok);
    const reader = response.body.getReader(), decoder = new TextDecoder();
    let pending = '', preparing = false;
    outer: while (true) {
      const chunk = await reader.read();
      assert(!chunk.done, 'Stream ended before initial frame count');
      pending += decoder.decode(chunk.value, { stream: true });
      let newline;
      while ((newline = pending.indexOf('\n')) >= 0) {
        const event = JSON.parse(pending.slice(0, newline)); pending = pending.slice(newline + 1);
        assert.notEqual(event.type, 'error', JSON.stringify(event));
        if (event.phase === 'preparing') preparing = true;
        if (event.phase === 'indexing') {
          assert(preparing); assert.equal(event.completed, 0); assert(event.total > 0);
          break outer;
        }
      }
    }
    controller.abort(); reader.cancel().catch(() => {});
  } finally {
    controller.abort();
    if (server.exitCode === null) {
      server.kill('SIGTERM');
      const force = setTimeout(() => server.kill('SIGKILL'), 1000);
      await new Promise(resolve => server.once('exit', resolve)); clearTimeout(force);
    }
    fs.rmSync(cache, { recursive: true, force: true });
  }
});
