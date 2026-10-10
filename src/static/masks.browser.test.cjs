const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const http = require('node:http');
const { spawn } = require('node:child_process');

test('frame preview overlays masks, changes prompt, and clears them when stepping', { timeout: 30000 }, async () => {
  const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'bquery-mask-browser-'));
  const frame = index => ({ type: 'frame', id: 'source-1-' + index, source: 'fixture.mp4', track_id: 1, sample_index: index, pts_seconds: index, duration_seconds: 1, size: 100, preview_url: '/frame.svg', raw_url: '/raw' });
  const seg = (...runs) => ({ count: runs.length, width: 4, height: 2, masks: runs.map(rle => ({ score: 0.9, rle })) });
  const rows = [{ frame: frame(1), segmentation: seg([0, 1, 7], [7, 1]), alternative: seg([0, 8]) }, { frame: frame(2), segmentation: seg() }];
  let report;
  const reported = new Promise(resolve => { report = resolve; });
  const browserCheck = `
    (async () => {
      const check = (condition, message) => { if (!condition) throw new Error(message); };
      const until = async fn => { for (let n = 0; n < 100; n++) { if (fn()) return; await new Promise(resolve => setTimeout(resolve, 20)); } throw new Error('Preview did not settle'); };
      try {
        await until(() => decoderAvailable);
        await runQuery();
        await until(() => !$('mask-controls').hidden);
        const canvas = $('mask-overlay'), context = canvas.getContext('2d');
        const pixel = (x, y) => Array.from(context.getImageData(x, y, 1, 1).data);
        check(canvas.width === 120 && canvas.height === 60, 'Preview scale');
        check(pixel(5, 5).join() === '67,224,173,255', 'First foreground pixel');
        check(pixel(115, 55).join() === '255,178,66,255', 'Second mask color / row-major order');
        check(pixel(60, 5)[3] === 0, 'Background transparency');
        check(getComputedStyle(canvas).opacity === '0.5', 'Default opacity');
        const box = canvas.getBoundingClientRect(), imageBox = $('preview').getBoundingClientRect();
        check(box.width === imageBox.width && box.height === imageBox.height, 'Overlay and image bounds');
        check(getComputedStyle(canvas).objectFit === getComputedStyle($('preview')).objectFit, 'Letterbox alignment');
        $('show-masks').checked = false; $('show-masks').dispatchEvent(new Event('change'));
        check(canvas.hidden, 'Toggle hides masks');
        $('show-masks').checked = true; $('show-masks').dispatchEvent(new Event('change'));
        check(!canvas.hidden, 'Toggle shows masks');
        $('mask-opacity').value = '0.25'; $('mask-opacity').dispatchEvent(new Event('input'));
        check(canvas.style.opacity === '0.25', 'Opacity control');
        $('mask-column').value = 'alternative'; $('mask-column').dispatchEvent(new Event('change'));
        check(pixel(60, 5)[3] === 255, 'Alternate segmentation alias');
        move(1);
        await until(() => $('mask-count').textContent === '0 masks' && !$('mask-controls').hidden);
        check(canvas.hidden, 'No stale masks on a frame without detections');
        document.querySelector('.frame-cell').click();
        await until(() => $('mask-count').textContent === '2 masks' && !canvas.hidden);
        check(pixel(60, 5)[3] === 0, 'Click restores the correct row masks');
        await fetch('/__report', { method: 'POST', body: JSON.stringify({ ok: true }) });
      } catch (error) { await fetch('/__report', { method: 'POST', body: JSON.stringify({ error: error.message }) }); }
    })();`;
  const assets = new Set(['app.js', 'graph.js', 'masks.js', 'style.css']);
  const server = http.createServer((req, res) => {
    const json = value => { res.setHeader('content-type', 'application/json'); res.end(JSON.stringify(value)); };
    if (req.url === '/__report') {
      let data = ''; req.on('data', chunk => { data += chunk; }); req.on('end', () => { report(JSON.parse(data)); res.end('ok'); }); return;
    }
    if (req.url === '/api/info') return json({ decoder_available: true, sources: [] });
    if (req.url === '/api/render') return json({ ready: true });
    if (req.url === '/api/query/stream') {
      res.setHeader('content-type', 'application/x-ndjson');
      return res.end([{ type: 'schema', columns: ['frame', 'segmentation', 'alternative'] }, { type: 'rows', rows }, { type: 'done' }].map(JSON.stringify).join('\n') + '\n');
    }
    if (req.url === '/frame.svg') { res.setHeader('content-type', 'image/svg+xml'); return res.end('<svg xmlns="http://www.w3.org/2000/svg" width="120" height="60"><rect width="120" height="60" fill="blue"/></svg>'); }
    if (req.url === '/overlay-check.js') { res.setHeader('content-type', 'text/javascript'); return res.end(browserCheck); }
    if (req.url === '/') {
      res.setHeader('content-type', 'text/html'); return res.end(fs.readFileSync(path.join(__dirname, 'index.html'), 'utf8').replace('</head>', '<script defer src="/overlay-check.js"></script></head>'));
    }
    const asset = req.url.slice(1);
    if (assets.has(asset)) { res.setHeader('content-type', asset.endsWith('.css') ? 'text/css' : 'text/javascript'); return res.end(fs.readFileSync(path.join(__dirname, asset))); }
    res.statusCode = 404; res.end();
  });
  let browser, timer;
  try {
    await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
    browser = spawn(process.env.BQUERY_TEST_FIREFOX || 'firefox', ['--headless', '--no-remote', '--profile', profile, 'http://127.0.0.1:' + server.address().port], { stdio: ['ignore', 'ignore', 'pipe'] });
    let errors = ''; browser.stderr.on('data', chunk => { errors += chunk; });
    const failure = new Promise((_, reject) => {
      browser.on('error', reject); browser.on('exit', code => reject(new Error('Firefox exited before verification: ' + code + ' ' + errors)));
      timer = setTimeout(() => reject(new Error('Browser check timed out: ' + errors)), 20000);
    });
    const outcome = await Promise.race([reported, failure]);
    assert.equal(outcome.error, undefined); assert.equal(outcome.ok, true);
  } finally {
    clearTimeout(timer);
    if (browser && browser.exitCode === null) { browser.kill('SIGTERM'); await new Promise(resolve => browser.once('exit', resolve)); }
    await new Promise(resolve => server.close(resolve));
    fs.rmSync(profile, { recursive: true, force: true });
  }
});
