"use strict";
// Numeric charts keep their SVG and append streamed rows locally.
window.ResultGraph = (() => {
  const finite = value => typeof value === "number" && Number.isFinite(value);
  const numericCache = new WeakMap(), views = new WeakMap();
  function numericColumns(result) {
    const signature = JSON.stringify(result.columns);
    let state = numericCache.get(result);
    if (!state || state.signature !== signature || state.rows !== result.rows || state.count > result.rows.length) {
      state = { signature, rows: result.rows, count: 0, names: new Set() }; numericCache.set(result, state);
    }
    for (; state.count < result.rows.length; state.count++) {
      const row = result.rows[state.count];
      for (const column of result.columns) if (finite(row[column])) state.names.add(column);
    }
    return result.columns.filter(column => state.names.has(column));
  }
  const numberLabel = value => {
    if (value === 0) return "0";
    if (Math.abs(value) >= 1e6 || Math.abs(value) < 0.0001) return value.toExponential(2);
    return Number(value.toPrecision(5)).toLocaleString("en-US", { maximumFractionDigits: 8 });
  };
  function defaults(columns) {
    const score = columns.find(column => /motion|score/i.test(column));
    if (!score && columns.includes("frame_index") && columns.includes("pts_seconds")) return { x: "frame_index", y: "pts_seconds" };
    const x = ["pts_seconds", "dts_seconds", "frame_index"].find(column => columns.includes(column)) || "";
    const y = (score !== x && score) || columns.find(column => column !== x) || columns[0] || "";
    return { x: y === x ? "" : x, y };
  }
  function emptyModel(x, y) {
    return { x, y, count: 0, points: [], plotted: [], segments: [], tail: null, minX: Infinity, maxX: -Infinity, minY: Infinity, maxY: -Infinity, hasFrames: false };
  }
  function range(min, max) {
    if (min === max) {
      const padding = min === 0 ? 0.5 : Math.max(Math.abs(min) * 0.05, Number.MIN_VALUE * 4);
      return [Math.max(-Number.MAX_VALUE, min - padding), Math.min(Number.MAX_VALUE, max + padding)];
    }
    return [min, max];
  }
  function append(chart, result) {
    const added = [];
    for (; chart.count < result.rows.length; chart.count++) {
      const index = chart.count, row = result.rows[index];
      const x = chart.x ? row[chart.x] : index + 1, y = row[chart.y];
      const frames = Object.values(row).filter(value => value && value.type === "frame");
      chart.hasFrames ||= frames.some(frame => frame.id);
      if (!finite(x) || !finite(y)) { chart.points.push(null); chart.tail = null; continue; }
      const source = row.path ?? frames[0]?.source, track = row.track_id ?? frames[0]?.track_id;
      const group = source == null && track == null ? "" : JSON.stringify([source ?? null, track ?? null]);
      const point = { x, y, index, group, hasFrame: frames.some(frame => frame.id) };
      chart.points.push(point); chart.plotted.push(point);
      if (!chart.tail || chart.tail[0].group !== group) { chart.tail = []; chart.segments.push(chart.tail); }
      const segment = chart.tail;
      // Usually streamed X values increase: append in O(1). Binary insertion
      // also handles unordered queries while retaining the original row ID.
      let slot = segment.length;
      if (slot && segment[slot - 1].x > x) {
        let low = 0, high = slot;
        while (low < high) { const middle = (low + high) >>> 1; if (segment[middle].x <= x) low = middle + 1; else high = middle; }
        slot = low;
      }
      segment.splice(slot, 0, point); added.push({ point, segment, slot });
      chart.minX = Math.min(chart.minX, x); chart.maxX = Math.max(chart.maxX, x);
      chart.minY = Math.min(chart.minY, y); chart.maxY = Math.max(chart.maxY, y);
    }
    chart.xRange = range(chart.minX, chart.maxX); chart.yRange = range(chart.minY, chart.maxY);
    return added;
  }
  function model(result, x, y) { const chart = emptyModel(x, y); append(chart, result); return chart; }
  function clear(container) { views.delete(container); container.replaceChildren(); }
  const width = 800, height = 380, left = 84, right = 24, top = 24, bottom = 60;
  const plotWidth = width - left - right, plotHeight = height - top - bottom;
  const unit = (value, bounds) => {
    const span = bounds[1] / 2 - bounds[0] / 2;
    return span ? (value / 2 - bounds[0] / 2) / span : (value - bounds[0]) / (bounds[1] - bounds[0]);
  };
  const px = (value, bounds) => left + unit(value, bounds) * plotWidth;
  const py = (value, bounds) => top + (1 - unit(value, bounds)) * plotHeight;
  function element(parent, name, attributes, text) {
    const node = document.createElementNS("http://www.w3.org/2000/svg", name);
    for (const [key, value] of Object.entries(attributes)) node.setAttribute(key, value);
    if (text != null) node.textContent = text;
    parent.append(node); return node;
  }
  function createView(container, chart, result, inspect) {
    container.replaceChildren();
    const svg = element(container, "svg", { viewBox: `0 0 ${width} ${height}`, role: "img" });
    const ticks = [];
    for (let tick = 0; tick <= 4; tick++) {
      const fraction = tick / 4, tx = left + fraction * plotWidth, ty = top + (1 - fraction) * plotHeight;
      element(svg, "line", { x1: left, x2: width - right, y1: ty, y2: ty, class: "graph-grid" });
      ticks.push([
        element(svg, "text", { x: tx, y: height - bottom + 24, "text-anchor": "middle", class: "graph-tick" }),
        element(svg, "text", { x: left - 12, y: ty + 4, "text-anchor": "end", class: "graph-tick" })
      ]);
    }
    element(svg, "text", { x: left + plotWidth / 2, y: height - 10, "text-anchor": "middle", class: "graph-axis-label" }, chart.x || "Row number");
    element(svg, "text", { transform: `translate(18 ${top + plotHeight / 2}) rotate(-90)`, "text-anchor": "middle", class: "graph-axis-label" }, chart.y);
    const plot = element(svg, "g", {}), lines = element(plot, "g", {}), dots = element(plot, "g", {});
    return { chart, rows: result.rows, svg, plot, lines, dots, ticks, inspect, baseX: chart.xRange.slice(), baseY: chart.yRange.slice(), segments: new Map(), markers: new Map() };
  }
  function geometry(view) {
    for (const [segment, line] of view.segments) {
      line.points.clear();
      for (const point of segment) { const position = view.svg.createSVGPoint(); position.x = px(point.x, view.baseX); position.y = py(point.y, view.baseY); line.points.appendItem(position); }
    }
    for (const [point, dot] of view.markers) { dot.setAttribute("cx", px(point.x, view.baseX)); dot.setAttribute("cy", py(point.y, view.baseY)); }
  }
  function render(container, result, x, y, inspect) {
    let view = views.get(container), chart, added;
    if (!view || view.rows !== result.rows || view.chart.x !== x || view.chart.y !== y || view.chart.count > result.rows.length || container.firstElementChild !== view.svg) {
      chart = emptyModel(x, y); added = append(chart, result);
      if (!chart.plotted.length) {
        clear(container);
        const empty = document.createElement("div"); empty.className = "graph-empty";
        empty.textContent = !result.rows.length ? "Run a query to plot its results." : !numericColumns(result).length ? "No numeric fields to plot. Select a timestamp, score, size, or other number." : "No rows have numeric values for both selected axes. Null values are skipped.";
        container.append(empty); return chart;
      }
      view = createView(container, chart, result, inspect); views.set(container, view);
    } else { chart = view.chart; added = append(chart, result); }
    let rebase = false;
    for (const { point, segment, slot } of added) {
      let line = view.segments.get(segment);
      if (!line) { line = element(view.lines, "polyline", { class: "graph-line" }); view.segments.set(segment, line); }
      const position = view.svg.createSVGPoint(), cx = px(point.x, view.baseX), cy = py(point.y, view.baseY);
      if (!finite(cx) || !finite(cy) || Math.abs(cx) > 1e30 || Math.abs(cy) > 1e30) { rebase = true; position.x = position.y = 0; }
      else { position.x = cx; position.y = cy; }
      line.points.insertItemBefore(position, slot);
      const description = `Row ${point.index + 1} · ${x || "Row number"}: ${numberLabel(point.x)} · ${y}: ${numberLabel(point.y)}`;
      const dot = element(view.dots, "ellipse", { cx: position.x, cy: position.y, rx: 4, ry: 4, class: "graph-point", "data-row": point.index, tabindex: 0, role: point.hasFrame ? "button" : "graphics-symbol", "aria-label": description + (point.hasFrame ? ". Preview frame" : "") });
      view.markers.set(point, dot); element(dot, "title", {}, description);
      if (point.hasFrame) {
        dot.classList.add("interactive");
        dot.addEventListener("click", () => view.inspect(point.index));
        dot.addEventListener("keydown", event => { if (event.key === "Enter" || event.key === " ") { event.preventDefault(); view.inspect(point.index); } });
      }
    }
    const transform = () => {
      const sx = (view.baseX[1] / 2 - view.baseX[0] / 2) / (chart.xRange[1] / 2 - chart.xRange[0] / 2);
      const sy = (view.baseY[1] / 2 - view.baseY[0] / 2) / (chart.yRange[1] / 2 - chart.yRange[0] / 2);
      return [sx, sy, px(view.baseX[0], chart.xRange) - sx * left, py(view.baseY[0], chart.yRange) - sy * (top + plotHeight)];
    };
    let [sx, sy, tx, ty] = transform();
    if (rebase || ![sx, sy, tx, ty].every(finite) || sx < 1e-6 || sy < 1e-6) {
      view.baseX = chart.xRange.slice(); view.baseY = chart.yRange.slice(); geometry(view);
      sx = sy = 1; tx = ty = 0;
    }
    view.plot.setAttribute("transform", `matrix(${sx} 0 0 ${sy} ${tx} ${ty})`);
    view.plot.style.setProperty("--point-rx", `${4 / sx}px`); view.plot.style.setProperty("--point-ry", `${4 / sy}px`);
    view.ticks.forEach(([xt, yt], tick) => {
      const fraction = tick / 4;
      xt.textContent = numberLabel(chart.xRange[0] * (1 - fraction) + chart.xRange[1] * fraction);
      yt.textContent = numberLabel(chart.yRange[0] * (1 - fraction) + chart.yRange[1] * fraction);
    });
    view.svg.setAttribute("aria-label", `${y} by ${x || "row number"}, ${chart.plotted.length} plotted rows`);
    return chart;
  }
  return { numericColumns, defaults, model, append, render, clear };
})();
