"use strict";
const MaskOverlay = (() => {
  const colors = [[67, 224, 173], [255, 178, 66], [103, 166, 255], [239, 111, 183], [185, 145, 255], [246, 229, 88]];
  function isSegmentation(value) {
    return value && typeof value === "object" && Number.isInteger(value.width) && Number.isInteger(value.height)
      && value.width > 0 && value.height > 0 && value.width * value.height <= 16 * 1024 * 1024
      && Array.isArray(value.masks) && value.masks.every(mask => mask && Array.isArray(mask.rle));
  }
  function columns(row) { return Object.entries(row || {}).filter(([, value]) => isSegmentation(value)); }
  function rasterize(segmentation) {
    if (!isSegmentation(segmentation)) throw new Error("Invalid segmentation dimensions or masks");
    const { width, height, masks } = segmentation, pixels = width * height;
    const data = new Uint8ClampedArray(pixels * 4);
    masks.forEach((mask, index) => {
      const color = colors[index % colors.length]; let offset = 0;
      mask.rle.forEach((length, run) => {
        if (!Number.isSafeInteger(length) || length < 0 || length > pixels - offset) throw new Error("Invalid mask run length");
        const end = offset + length;
        // Runs start with background, including a possible zero-length run.
        if (run % 2 === 1) for (let pixel = offset; pixel < end; pixel++) {
          const byte = pixel * 4; data[byte] = color[0]; data[byte + 1] = color[1]; data[byte + 2] = color[2]; data[byte + 3] = 255;
        }
        offset = end;
      });
      if (offset !== pixels) throw new Error("Mask runs do not cover the complete image");
    });
    return { width, height, data };
  }
  function draw(canvas, image, segmentation) {
    const pixels = rasterize(segmentation), mask = document.createElement("canvas");
    mask.width = pixels.width; mask.height = pixels.height;
    const source = mask.getContext("2d"), rgba = source.createImageData(pixels.width, pixels.height);
    rgba.data.set(pixels.data); source.putImageData(rgba, 0, 0);
    canvas.width = image.naturalWidth; canvas.height = image.naturalHeight;
    const target = canvas.getContext("2d"); target.imageSmoothingEnabled = false;
    target.drawImage(mask, 0, 0, canvas.width, canvas.height);
  }
  return { columns, rasterize, draw };
})();
if (typeof module !== "undefined") module.exports = MaskOverlay;
