const test = require('node:test');
const assert = require('node:assert/strict');
const masks = require('./masks.js');
const segmentation = (width, height, ...rle) => ({ width, height, masks: rle.map(rle => ({ score: 0.9, rle })) });
const alpha = image => Array.from(image.data).filter((_, i) => i % 4 === 3);

test('row-major RLE starts in background and accepts a zero first run', () => {
  const image = masks.rasterize(segmentation(3, 2, [0, 2, 2, 2]));
  assert.deepEqual(alpha(image), [255, 255, 0, 0, 255, 255]);
  assert.deepEqual(alpha(masks.rasterize(segmentation(3, 2, [6]))), [0, 0, 0, 0, 0, 0]);
  assert.deepEqual(alpha(masks.rasterize(segmentation(3, 2))), [0, 0, 0, 0, 0, 0]);
});
test('each mask gets a color, without erasing earlier foreground', () => {
  const image = masks.rasterize(segmentation(2, 2, [0, 1, 3], [3, 1]));
  assert.deepEqual(alpha(image), [255, 0, 0, 255]);
  assert.notDeepEqual(Array.from(image.data.slice(0, 3)), Array.from(image.data.slice(12, 15)));
});
test('rejects invalid lengths and dimensions before rendering', () => {
  for (const rle of [[-1, 5], [0, 5], [0, 3], [0, 1.5, 2.5], [NaN], [Infinity]]) {
    assert.throws(() => masks.rasterize(segmentation(2, 2, rle)));
  }
  assert.throws(() => masks.rasterize(segmentation(0, 2, [])));
  assert.throws(() => masks.rasterize(segmentation(1000000, 1000000, [])));
});
test('discovers segmentation columns regardless of alias', () => {
  const value = segmentation(2, 2, [0, 4]);
  assert.deepEqual(masks.columns({ frame: { type: 'frame' }, hats: value, other: 2 }), [['hats', value]]);
});
test('scales from mask dimensions to the decoded preview with nearest pixels', () => {
  const calls = [];
  const source = { createImageData(w, h) { return { data: new Uint8ClampedArray(w * h * 4) }; }, putImageData(image) { calls.push(alpha(image)); } };
  const offscreen = { getContext: () => source };
  global.document = { createElement() { return offscreen; } };
  const context = { drawImage(...args) { calls.push(args); } };
  const canvas = { getContext: () => context };
  try { masks.draw(canvas, { naturalWidth: 1280, naturalHeight: 720 }, segmentation(3, 2, [0, 2, 2, 2])); }
  finally { delete global.document; }
  assert.equal(canvas.width, 1280); assert.equal(canvas.height, 720);
  assert.equal(offscreen.width, 3); assert.equal(offscreen.height, 2);
  assert.equal(context.imageSmoothingEnabled, false);
  assert.deepEqual(calls[0], [255, 255, 0, 0, 255, 255]);
  assert.deepEqual(calls[1], [offscreen, 0, 0, 1280, 720]);
});
