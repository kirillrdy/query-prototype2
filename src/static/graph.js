"use strict";
// Numeric result charts are rendered locally, without fetching libraries or data.
window.ResultGraph = (() => {
  const finite = value => typeof value === "number" && Number.isFinite(value);
  const numericColumns = result => result.columns.filter(column => result.rows.some(row => finite(row[column])));
  const numberLabel = value => {
    if (value === 0) return "0";
    if (Math.abs(value) >= 1e6 || Math.abs(value) < 0.0001) return value.toExponential(2);
    return Number(value.toPrecision(5)).toLocaleString("en-US", { maximumFractionDigits: 8 });
  };
  function defaults(columns) {
    const x = ["pts_seconds", "dts_seconds", "sample_index"].find(column => columns.includes(column)) || "";
    const y = columns.find(column => /motion|score/i.test(column) && column !== x) || columns.find(column => column !== x) || columns[0] || "";
    return { x: y === x ? "" : x, y };
  }
  function model(result, x, y) {
    const points = result.rows.map((row, index) => {
      const px = x ? row[x] : index + 1, py = row[y];
      const frame = Object.values(row).find(value => value && value.type === "frame");
      const source = row.path ?? frame?.source;
      const track = row.track_id ?? frame?.track_id;
      const group = source == null && track == null ? "" : JSON.stringify([source ?? null, track ?? null]);
      return finite(px) && finite(py) ? { x: px, y: py, index, group } : null;
    });
    const plotted = points.filter(Boolean);
    const extent = field => {
      let min = Infinity, max = -Infinity;
      for (const point of plotted) { min = Math.min(min, point[field]); max = Math.max(max, point[field]); }
      if (min === max) {
        const padding = min === 0 ? 0.5 : Math.max(Math.abs(min) * 0.05, Number.MIN_VALUE * 4);
        min = Math.max(-Number.MAX_VALUE, min - padding); max = Math.min(Number.MAX_VALUE, max + padding);
      }
      return [min, max];
    };
    return { points, plotted, xRange: extent("x"), yRange: extent("y") };
  }
  function render(container, result, x, y, inspect) {
    container.replaceChildren();
    const chart = model(result, x, y);
    if (!chart.plotted.length) {
      const empty = document.createElement("div"); empty.className = "graph-empty";
      empty.textContent = !result.rows.length ? "Run a query to plot its results." : !numericColumns(result).length ? "No numeric fields to plot. Select a timestamp, score, size, or other number." : "No rows have numeric values for both selected axes. Null values are skipped.";
      container.append(empty); return chart;
    }
    const width = 800, height = 380, left = 84, right = 24, top = 24, bottom = 60;
    const plotWidth = width - left - right, plotHeight = height - top - bottom;
    // Dividing before subtraction avoids overflow for very large finite values.
    const unit = (value, range) => (value / 2 - range[0] / 2) / (range[1] / 2 - range[0] / 2);
    const px = value => left + unit(value, chart.xRange) * plotWidth;
    const py = value => top + (1 - unit(value, chart.yRange)) * plotHeight;
    const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
    svg.setAttribute("viewBox", `0 0 ${width} ${height}`); svg.setAttribute("role", "img");
    svg.setAttribute("aria-label", `${y} by ${x || "row number"}, ${chart.plotted.length} plotted rows`);
    const element = (name, attributes, text) => {
      const node = document.createElementNS(svg.namespaceURI, name);
      for (const [key, value] of Object.entries(attributes)) node.setAttribute(key, value);
      if (text != null) node.textContent = text;
      svg.append(node); return node;
    };
    for (let tick = 0; tick <= 4; tick++) {
      const fraction = tick / 4, tx = left + fraction * plotWidth, ty = top + (1 - fraction) * plotHeight;
      element("line", { x1: left, x2: width - right, y1: ty, y2: ty, class: "graph-grid" });
      element("text", { x: left - 12, y: ty + 4, "text-anchor": "end", class: "graph-tick" }, numberLabel(chart.yRange[0] * (1 - fraction) + chart.yRange[1] * fraction));
      element("text", { x: tx, y: height - bottom + 24, "text-anchor": "middle", class: "graph-tick" }, numberLabel(chart.xRange[0] * (1 - fraction) + chart.xRange[1] * fraction));
    }
    element("text", { x: left + plotWidth / 2, y: height - 10, "text-anchor": "middle", class: "graph-axis-label" }, x || "Row number");
    element("text", { transform: `translate(18 ${top + plotHeight / 2}) rotate(-90)`, "text-anchor": "middle", class: "graph-axis-label" }, y);
    // A missing value breaks the line, rather than treating a null score as zero.
    let path = "", previous = null;
    for (const point of chart.points) {
      if (!point) { previous = null; continue; }
      path += `${previous && previous.group === point.group ? "L" : "M"}${px(point.x)},${py(point.y)} `;
      previous = point;
    }
    element("path", { d: path, class: "graph-line" });
    for (const point of chart.plotted) {
      const description = `Row ${point.index + 1} · ${x || "Row number"}: ${numberLabel(point.x)} · ${y}: ${numberLabel(point.y)}`;
      const hasFrame = Object.values(result.rows[point.index]).some(value => value && value.type === "frame" && value.id);
      const dot = element("circle", { cx: px(point.x), cy: py(point.y), r: 4, class: "graph-point", "data-row": point.index, tabindex: 0, role: hasFrame ? "button" : "graphics-symbol", "aria-label": description + (hasFrame ? ". Preview frame" : "") });
      const title = document.createElementNS(svg.namespaceURI, "title"); title.textContent = description; dot.append(title);
      if (hasFrame) {
        dot.classList.add("interactive");
        dot.addEventListener("click", () => inspect(point.index));
        dot.addEventListener("keydown", event => { if (event.key === "Enter" || event.key === " ") { event.preventDefault(); inspect(point.index); } });
      }
    }
    container.append(svg); return chart;
  }
  return { numericColumns, defaults, model, render };
})();
