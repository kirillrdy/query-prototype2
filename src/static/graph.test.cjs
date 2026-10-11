const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

const context = { window: {} };
vm.runInNewContext(fs.readFileSync(__dirname + '/graph.js', 'utf8'), context);
const graph = context.window.ResultGraph;
const plain = value => JSON.parse(JSON.stringify(value));

test('timestamp versus frame index defaults to index on X, preserving motion defaults', () => {
  assert.deepEqual(plain(graph.defaults(['frame_index', 'pts_seconds'])), { x: 'frame_index', y: 'pts_seconds' });
  assert.deepEqual(plain(graph.defaults(['frame_index', 'pts_seconds', 'motion_score'])), { x: 'pts_seconds', y: 'motion_score' });
});

test('unordered query rows draw in X order while retaining correct frame identities and values', () => {
  const result = { columns: ['frame_index', 'pts_seconds'], rows: [
    { frame_index: 7, pts_seconds: 0.333667 },
    { frame_index: 5, pts_seconds: 0.250250 },
    { frame_index: 4, pts_seconds: 0.208542 },
    { frame_index: 6, pts_seconds: 0.291958 },
  ] };
  const original = structuredClone(result);
  const chart = graph.model(result, 'pts_seconds', 'frame_index');
  assert.deepEqual(plain(chart.segments[0].map(point => point.index)), [2, 1, 3, 0]);
  assert.deepEqual(plain(chart.segments[0].map(point => point.y)), [4, 5, 6, 7]);
  assert.deepEqual(plain(chart.plotted.map(point => point.index)), [0, 1, 2, 3]);
  assert.deepEqual(result, original);
});

test('sorting lines keeps null gaps and source/track boundaries', () => {
  const result = { columns: ['x', 'y'], rows: [
    { x: 2, y: 20, path: 'a', track_id: 1 },
    { x: 1, y: 10, path: 'a', track_id: 1 },
    { x: 0, y: null, path: 'a', track_id: 1 },
    { x: 0, y: 0, path: 'a', track_id: 1 },
    { x: 2, y: 40, path: 'a', track_id: 2 },
    { x: 1, y: 30, path: 'b', track_id: 2 },
  ] };
  assert.deepEqual(plain(graph.model(result, 'x', 'y').segments.map(segment => segment.map(point => point.index))), [[1, 0], [3], [4], [5]]);
});
