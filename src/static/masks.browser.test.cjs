const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const http = require('node:http');
const { spawn } = require('node:child_process');

test('frame preview overlays masks, changes prompt, and clears them when stepping', { timeout: 30000 }, async () => {
  const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'bquery-mask-browser-'));
  const frame = index => ({ type: 'frame', id: 'source-1-' + index, source: 'fixture.mp4', track_id: 1, frame_index: index, pts_seconds: index, duration_seconds: 1, size: 100, preview_url: '/frame.svg?index=' + index, raw_url: '/raw' });
  const seg = (...runs) => ({ count: runs.length, width: 4, height: 2, masks: runs.map(rle => ({ score: 0.9, rle })) });
  const rows = [{ frame: frame(1), segmentation: seg([0, 1, 7], [7, 1]), alternative: seg([0, 8]) }, { frame: frame(2), segmentation: seg() }];
  const requested = [], held = [];
  let renderCalls = 0;
  const image = res => { res.setHeader('content-type', 'image/svg+xml'); res.end('<svg xmlns="http://www.w3.org/2000/svg" width="120" height="60"><rect width="120" height="60" fill="blue"/></svg>'); };
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
        check(!$('table-view').hidden && $('player-view').hidden, 'Results stay in Table until Player is selected');
        check($('player-view').parentElement.classList.contains('result-panel'), 'Player lives beside the other result panels');
        $('view-player').click();
        check(!$('player-view').hidden && $('table-view').hidden && $('graph-view').hidden, 'Player tab visibility');
        check($('view-player').getAttribute('aria-selected') === 'true', 'Player selected tab state');
        check(!$('player-details').open && $('frame-title').closest('details') === $('player-details'), 'Extra frame details start collapsed');
        check($('scrub').closest('.transport') && $('play').textContent === '▶ Play', 'Compact playback bar');
        $('player-details').open = true;
        check($('speed').getBoundingClientRect().width > 0 && $('download').getBoundingClientRect().width > 0, 'Extra controls remain available');
        $('player-details').open = false;
        const canvas = $('mask-overlay'), context = canvas.getContext('2d');
        const pixel = (x, y) => Array.from(context.getImageData(x, y, 1, 1).data);
        check(canvas.width === 120 && canvas.height === 60, 'Preview scale');
        check(pixel(5, 5).join() === '67,224,173,255', 'First foreground pixel');
        check(pixel(115, 55).join() === '255,178,66,255', 'Second mask color / row-major order');
        check(pixel(60, 5)[3] === 0, 'Background transparency');
        check(getComputedStyle(canvas).opacity === '0.5', 'Default opacity');
        const box = canvas.getBoundingClientRect(), imageBox = $('preview').getBoundingClientRect();
        check(box.width > 0 && box.width === imageBox.width && box.height === imageBox.height, 'Overlay and image bounds: ' + JSON.stringify({ overlay: box.toJSON(), image: imageBox.toJSON() }));
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
        $('view-graph').click();
        check(!$('graph-view').hidden && $('player-view').hidden, 'Graph tab visibility');
        $('view-table').click();
        check(!$('table-view').hidden && $('player-view').hidden && queue.length === 2, 'Table preserves selected frames');
        document.querySelector('.frame-cell').click();
        await until(() => $('mask-count').textContent === '2 masks' && !canvas.hidden);
        check(!$('player-view').hidden, 'Frame click opens Player');
        $('view-player').dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowLeft' }));
        check(!$('graph-view').hidden && document.activeElement === $('view-graph'), 'Keyboard switches result tabs');
        $('view-graph').dispatchEvent(new KeyboardEvent('keydown', { key: 'End' }));
        check(!$('player-view').hidden && document.activeElement === $('view-player'), 'Keyboard selects Player');
        $('play').click(); await until(() => playing);
        $('view-table').click();
        check(!playing && $('player-view').hidden, 'Leaving Player pauses playback');
        // A previous graph can contain more markers than the next query has
        // rows. Preview highlighting must not read those stale row indices.
        result.columns = ['frame', 'pts_seconds'];
        result.rows.forEach((row, index) => { row.pts_seconds = index; });
        setResultView('graph');
        check($('graph-plot').querySelectorAll('.graph-point').length === 2, 'Previous query graph markers');
        setResultView('player');
        $('sql').value = "SELECT frame, sam3(frame, 'hat') AS segmentation FROM frames WHERE frame_index = 3000 LIMIT 1";
        await runQuery();
        await until(() => !$('mask-controls').hidden || $('message').classList.contains('error'));
        check(!$('message').classList.contains('error'), 'Single-frame query preview: ' + $('message').textContent);
        check(result.rows.length === 1 && $('mask-count').textContent === '1 mask', 'New query mask preview');
        inspectGraphRow(1); // A queued click from an old chart is harmless.
        setResultView('graph');
        check(!$('graph-plot').querySelector('.graph-point'), 'Graph shows only the new query results');
        // Later previews stay blocked while the first selected frame plays.
        $('sql').value = 'SELECT frame FROM frames WHERE frame_index >= 4000 LIMIT 43';
        await runQuery();
        await until(() => $('frame-badge').textContent === 'PTS 4000.000 s');
        const playback = play();
        await until(() => playing && !$('player-view').hidden);
        await playback;
        check($('message').textContent === 'Playing 43 selected frames.', 'Playback starts without preparing the selection');
        const status = await (await fetch('/__status')).json();
        check(status.renderCalls === 0, 'No bulk render requests');
        check(status.requested.includes(4000) && status.requested.every(index => index <= 4002), 'Only the current frame and bounded lookahead are requested');
        stop();
        await fetch('/__release');
        await new Promise(resolve => setTimeout(resolve, 1100));
        check(!playing && position === 0 && $('frame-badge').textContent === 'PTS 4000.000 s', 'Paused playback does not advance when buffered frames arrive');
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
    if (req.url === '/api/render') { renderCalls++; return json({ ready: true }); }
    if (req.url === '/__status') return json({ renderCalls, requested });
    if (req.url === '/__release') { held.splice(0).forEach(image); return json({ ready: true }); }
    if (req.url === '/api/query/stream') {
      let data = ''; req.on('data', chunk => { data += chunk; });
      req.on('end', () => {
        const singleFrame = JSON.parse(data).sql.includes('frame_index = 3000');
        const streaming = JSON.parse(data).sql.includes('frame_index >= 4000');
        const selected = streaming ? Array.from({ length: 43 }, (_, index) => ({ frame: frame(4000 + index) })) : singleFrame ? [{ frame: frame(3000), segmentation: seg([0, 4, 4]) }] : rows;
        res.setHeader('content-type', 'application/x-ndjson');
        res.end([{ type: 'schema', columns: streaming ? ['frame'] : singleFrame ? ['frame', 'segmentation'] : ['frame', 'segmentation', 'alternative'] }, { type: 'rows', rows: selected }, { type: 'done' }].map(JSON.stringify).join('\n') + '\n');
      }); return;
    }
    if (req.url.startsWith('/frame.svg?index=')) {
      const index = Number(new URL(req.url, 'http://localhost').searchParams.get('index'));
      if (index >= 4000) requested.push(index);
      if (index > 4000) { held.push(res); return; }
      return image(res);
    }
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
    server.closeAllConnections();
    await new Promise(resolve => server.close(resolve));
    fs.rmSync(profile, { recursive: true, force: true });
  }
});
