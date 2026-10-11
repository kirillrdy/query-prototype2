"use strict";
const $ = id => document.getElementById(id);
const examples = {
  sam3: "SELECT frame, sam3(frame, 'hat') AS segmentation\nFROM frames('filename.mp4')\nWHERE is_keyframe = true\nLIMIT 1",
  people: "SELECT frame, path, pts_seconds, person_count(frame) AS people\nFROM frames('filename.mp4')\nWHERE frame_index <= 100\n  AND person_count(frame) > 0\nORDER BY path, pts_seconds",
	 motion: "SELECT frame, pts_seconds,\n  motion(frame, LAG(frame) OVER (\n    PARTITION BY path, track_id\n    ORDER BY pts_seconds, frame_index\n  )) AS motion_score\nFROM frames('filename.mp4')\nORDER BY path, track_id, pts_seconds\nLIMIT 100",
  mixed: "SELECT frame, path, frame_index, pts_seconds\nFROM frames('filename.mp4')\nLIMIT 48",
  keyframes: "SELECT frame, path, pts_seconds, size\nFROM frames('filename.mp4')\nWHERE is_keyframe = true\nLIMIT 100",
  tracks: "SELECT path, track_id, handler, codec, width, height\nFROM tracks('filename.mp4')"
};
let result = { columns: [], rows: [] }, selection = new Set(), queue = [], position = 0;
let generation = 0, previewVersion = 0, playbackVersion = 0, playing = false, decoderAvailable = false;
const images = new Map();
const frameRows = new Map(), queueRows = new Map();
let currentMasks = null;
let queryController = null;
let resultView = "table", graphAxesChosen = false, graphColumns = "";
const frameValues = row => Object.values(row || {}).filter(v => v && typeof v === "object" && v.type === "frame" && v.id);
function message(text, kind = "") { $("message").textContent = text; $("message").className = "message " + kind; }
function text(tag, value, className) { const el = document.createElement(tag); el.textContent = value; if (className) el.className = className; return el; }
async function api(path) {
  const response = await fetch(path);
  const data = await response.json(); if (!response.ok) throw new Error(data.error || "Request failed"); return data;
}
async function refreshInfo() {
  const info = await api("/api/info"); decoderAvailable = info.decoder_available;
  $("decoder-note").hidden = decoderAvailable;
  return info;
}
function stop() { playing = false; playbackVersion++; previewVersion++; $("play").textContent = "▶ Play"; }
function updateQueue(stopPlayback = true) {
  if (stopPlayback) stop(); const unique = new Map();
  queueRows.clear();
  result.rows.forEach((row, index) => { if (selection.has(index)) for (const f of frameValues(row)) { unique.set(f.id, f); queueRows.set(f.id, row); } });
  queue = [...unique.values()];
  if ($("order").value === "presentation") {
    const groups = new Map(); for (const f of queue) { const key = f.id.split("-")[0] + ":" + f.track_id; if (!groups.has(key)) groups.set(key, groups.size); }
    const group = f => groups.get(f.id.split("-")[0] + ":" + f.track_id);
    queue.sort((a, b) => group(a) - group(b) || a.pts_seconds - b.pts_seconds || a.frame_index - b.frame_index);
  }
  position = Math.min(position, Math.max(0, queue.length - 1));
  $("selection-count").textContent = queue.length + " selected";
  $("play").disabled = !queue.length || !decoderAvailable;
  $("previous").disabled = $("next").disabled = $("scrub").disabled = !queue.length;
  $("scrub").max = Math.max(0, queue.length - 1); updatePosition();
}
function updatePosition() { $("scrub").value = position; $("position").textContent = queue.length ? `${position + 1} / ${queue.length}` : "0 / 0"; }
function renderTable() {
  const head = $("results").querySelector("thead"), body = $("results").querySelector("tbody"); head.replaceChildren(); body.replaceChildren();
  $("empty").hidden = result.rows.length > 0; $("row-count").textContent = result.rows.length;
  if (!result.rows.length) { $("empty").querySelector("h3").textContent = "No matching rows"; $("empty").querySelector("p").textContent = "Try a wider selection or another table."; }
  const hasFrames = result.rows.some(row => frameValues(row).length);
  const hr = document.createElement("tr");
  if (hasFrames) {
    const th = document.createElement("th"), all = document.createElement("input"); all.type = "checkbox"; all.checked = selection.size === result.rows.filter(row => frameValues(row).length).length; all.indeterminate = selection.size > 0 && !all.checked; all.setAttribute("aria-label", "Select all frame rows");
    all.addEventListener("change", () => {
      selection.clear(); if (all.checked) result.rows.forEach((row, i) => { if (frameValues(row).length) selection.add(i); });
      body.querySelectorAll("input[type=checkbox]").forEach(c => { c.checked = selection.has(Number(c.dataset.row)); }); updateQueue();
    }); th.append(all); hr.append(th);
  }
  for (const c of result.columns) hr.append(text("th", c)); head.append(hr);
  result.rows.forEach((row, index) => {
    const tr = document.createElement("tr"); tr.dataset.row = index;
    if (hasFrames) {
      const td = document.createElement("td"); if (frameValues(row).length) {
        const checkbox = document.createElement("input"); checkbox.type = "checkbox"; checkbox.checked = selection.has(index); checkbox.dataset.row = index; checkbox.setAttribute("aria-label", `Select frame row ${index + 1}`);
        checkbox.addEventListener("change", () => { if (checkbox.checked) selection.add(index); else selection.delete(index); allState(); updateQueue(); }); td.append(checkbox);
      } tr.append(td);
    }
    for (const c of result.columns) {
      const td = document.createElement("td"), value = row[c];
      if (value && value.type === "frame") {
        const button = text("button", `▣ Frame ${value.frame_index}`, "frame-cell"); button.title = `${value.size} encoded bytes · preview frame`; button.dataset.frame = value.id;
        button.addEventListener("click", () => { stop(); const p = queue.findIndex(f => f.id === value.id); if (p >= 0) { position = p; updatePosition(); } show(value, false, row).catch(e => message(e.message, "error")); }); td.append(button);
      } else if (value == null) td.append(text("span", "null", "null"));
      else td.textContent = typeof value === "object" ? JSON.stringify(value) : typeof value === "number" && !Number.isInteger(value) ? Number(value.toFixed(6)).toString() : String(value);
      tr.append(td);
    } body.append(tr);
  });
  function allState() { const all = head.querySelector("input"); if (!all) return; const count = result.rows.filter(r => frameValues(r).length).length; all.checked = selection.size === count; all.indeterminate = selection.size > 0 && selection.size < count; }
}
function renderResults() { renderTable(); renderGraph(); }
function inspectGraphRow(index) {
  const frame = frameValues(result.rows[index])[0]; if (!frame) return;
  stop(); const next = queue.findIndex(value => value.id === frame.id);
  if (next >= 0) { position = next; updatePosition(); }
  show(frame, false, result.rows[index]).catch(error => message(error.message, "error"));
}
function renderGraph() {
  const columns = ResultGraph.numericColumns(result), signature = JSON.stringify(columns);
  const x = $("graph-x"), y = $("graph-y"), defaults = ResultGraph.defaults(columns);
  if (signature !== graphColumns) {
    const previousX = x.value, previousY = y.value;
    graphColumns = signature; x.replaceChildren(); y.replaceChildren();
    const option = (value, label) => { const node = text("option", label); node.value = value; return node; };
    x.append(option("", "Row number"));
    for (const column of columns) { x.append(option(column, column)); y.append(option(column, column)); }
    if (graphAxesChosen && columns.includes(previousY) && (!previousX || columns.includes(previousX))) { x.value = previousX; y.value = previousY; }
    else { graphAxesChosen = false; }
  }
  if (!graphAxesChosen) { x.value = defaults.x; y.value = defaults.y; }
  x.disabled = y.disabled = !columns.length;
  if (resultView !== "graph") {
    // Hidden markers must not retain indices into an earlier query's rows.
    $("graph-plot").replaceChildren(); $("graph-note").textContent = "";
    return;
  }
  const chart = ResultGraph.render($("graph-plot"), result, x.value, y.value, inspectGraphRow);
  $("graph-note").textContent = chart.plotted.length ? `${chart.plotted.length} points · hover for values${result.rows.some(row => frameValues(row).length) ? " · click to preview" : ""}` : "";
}
function setResultView(view) {
  resultView = view;
  if (view !== "player") stop();
  for (const name of ["table", "graph", "player"]) {
    const active = view === name;
    $(name + "-view").hidden = !active;
    $("view-" + name).setAttribute("aria-selected", String(active));
    $("view-" + name).tabIndex = active ? 0 : -1;
  }
  renderGraph();
}
function loadImage(frame) {
  if (images.has(frame.id)) return images.get(frame.id);
  const promise = new Promise((resolve, reject) => { const img = new Image(); img.onload = () => resolve(img); img.onerror = () => reject(new Error("Could not load frame preview")); img.src = frame.preview_url; });
  images.set(frame.id, promise); if (images.size > 8) images.delete(images.keys().next().value);
  promise.catch(() => images.delete(frame.id)); return promise;
}
function clearMasks() {
  currentMasks = null; $("mask-overlay").hidden = true; $("mask-controls").hidden = true;
}
function maskVisibility() {
  const enabled = currentMasks && $("show-masks").checked && currentMasks.segmentation.masks.length > 0;
  $("mask-overlay").hidden = !enabled; $("mask-overlay").style.opacity = $("mask-opacity").value;
  $("mask-opacity").disabled = !enabled;
}
function drawMasks() {
  if (!currentMasks) return;
  const segmentation = currentMasks.columns.find(([name]) => name === $("mask-column").value)[1];
  currentMasks.segmentation = segmentation;
  $("mask-overlay").hidden = true;
  MaskOverlay.draw($("mask-overlay"), currentMasks.image, segmentation);
  $("mask-count").textContent = `${segmentation.masks.length} ${segmentation.masks.length === 1 ? "mask" : "masks"}`;
  maskVisibility();
}
function showMasks(image, row) {
  const columns = MaskOverlay.columns(row);
  // A segmentation has no frame identifier. Avoid overlaying it on an
  // unrelated reference when a row contains different frame values.
  if (!columns.length || new Set(frameValues(row).map(frame => frame.id)).size !== 1) return;
  const choice = $("mask-column").value; $("mask-column").replaceChildren();
  for (const [name] of columns) { const option = text("option", name); option.value = name; $("mask-column").append(option); }
  if (columns.some(([name]) => name === choice)) $("mask-column").value = choice;
  $("mask-column-label").hidden = columns.length < 2;
  currentMasks = { image, columns }; $("mask-controls").hidden = false; drawMasks();
}
async function show(frame, quiet = false, row = queueRows.get(frame.id) || frameRows.get(frame.id)) {
  const version = ++previewVersion;
  if (!quiet) setResultView("player");
  clearMasks();
  $("download").href = frame.raw_url; $("download").hidden = false;
  $("frame-title").textContent = `${frame.source} · Frame ${frame.frame_index}`;
  $("frame-detail").textContent = `Track ${frame.track_id} · PTS ${frame.pts_seconds.toFixed(6)} s · ${frame.size.toLocaleString()} bytes`;
  $("results").querySelectorAll("tbody tr").forEach(tr => tr.classList.toggle("active", frameValues(result.rows[Number(tr.dataset.row)]).some(f => f.id === frame.id)));
  $("graph-plot").querySelectorAll(".graph-point").forEach(point => point.classList.toggle("active", frameValues(result.rows[Number(point.dataset.row)]).some(f => f.id === frame.id)));
  if (!decoderAvailable) return;
  if (!images.has(frame.id) && !quiet && !playing) message(`Rendering frame ${frame.frame_index}…`, "busy");
  const img = await loadImage(frame); if (version !== previewVersion) return;
  $("preview").src = img.src; $("preview").hidden = false; $("preview-empty").hidden = true;
  showMasks(img, row);
  $("frame-badge").textContent = `PTS ${frame.pts_seconds.toFixed(3)} s`; $("frame-badge").hidden = false;
  if (!playing && !quiet) message("Frame ready. Select rows to create a playback sequence.");
}
async function readQueryStream(sql, signal, receive) {
  const response = await fetch("/api/query/stream", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ sql }), signal });
  if (!response.ok) { const data = await response.json(); throw new Error(data.error || "Query failed"); }
  if (!response.body) throw new Error("Streaming responses are unavailable in this browser");
  const reader = response.body.getReader(), decoder = new TextDecoder(); let pending = "", completed = false;
  const line = value => {
    if (!value.trim()) return;
    const event = JSON.parse(value);
    if (event.type === "error") throw new Error(event.error || "Query failed");
    if (event.type === "done") completed = true;
    receive(event);
  };
  try {
    while (true) {
      const { value, done } = await reader.read(); pending += done ? decoder.decode() : decoder.decode(value, { stream: true });
      let newline; while ((newline = pending.indexOf("\n")) >= 0) { line(pending.slice(0, newline)); pending = pending.slice(newline + 1); }
      if (done) break;
    }
    line(pending); if (!completed) throw new Error("Query stream ended before completion");
  } finally { await reader.cancel().catch(() => {}); reader.releaseLock(); }
}
async function runQuery() {
  if (queryController) queryController.abort();
  const controller = new AbortController(); queryController = controller;
  const version = ++generation, started = performance.now(); let firstRow = null;
  previewVersion++; stop(); $("run").disabled = true; $("cancel-query").hidden = false; message("Running query…", "busy");
  result = { columns: [], rows: [] }; graphAxesChosen = false; selection.clear(); renderResults(); updateQueue();
  frameRows.clear(); clearMasks();
  $("preview").hidden = true; $("preview-empty").hidden = false; $("frame-badge").hidden = true; $("download").hidden = true;
  $("frame-title").textContent = "No frame selected"; $("frame-detail").textContent = "Binary frame + decoded preview";
  const timing = () => firstRow == null ? "" : ` · first row in ${(firstRow / 1000).toFixed(2)} s`;
  try {
    await readQueryStream($("sql").value, controller.signal, event => {
      if (version !== generation) return;
      if (event.type === "schema") {
        result.columns = event.columns; renderResults(); $("result-note").textContent = "Preparing query input…";
      } else if (event.type === "rows") {
        const first = result.rows.length === 0;
        for (const row of event.rows) { const index = result.rows.length; result.rows.push(row); const frames = frameValues(row); if (frames.length) selection.add(index); for (const frame of frames) if (!frameRows.has(frame.id)) frameRows.set(frame.id, row); }
        if (firstRow == null && event.rows.length) firstRow = performance.now() - started;
        renderResults(); updateQueue(false);
        const note = $("result-note");
        if (!note.querySelector(".spinner")) {
          const spinner = text("span", "", "spinner"); spinner.setAttribute("aria-hidden", "true");
          note.replaceChildren(spinner, text("span", "Receiving rows", "sr-only"));
        }
        message(`${result.rows.length} rows received${timing()}…`, "busy");
        if (first && queue.length) show(queue[0], true).catch(error => { if (version === generation) message(`Preview: ${error.message}`, "error"); });
      } else if (event.type === "done") {
        $("result-note").textContent = queue.length ? "Select rows for playback · click a frame to inspect" : "Scalar query results";
        message(`${result.rows.length} rows returned${timing()}.`);
      }
    });
    if (version === generation) await refreshInfo().catch(() => {});
  } catch (error) {
    if (version === generation) {
      const canceled = controller.signal.aborted;
      $("result-note").textContent = `${canceled ? "Canceled" : "Incomplete query"} · ${result.rows.length} rows received`;
      message(canceled ? `Query canceled. ${result.rows.length} received rows remain available.` : `${error.message} (${result.rows.length} rows received; result incomplete)`, canceled ? "" : "error");
    }
  } finally {
    if (version === generation) { $("run").disabled = false; $("cancel-query").hidden = true; queryController = null; }
  }
}

async function play() {
  if (playing) { stop(); return; } if (!queue.length) return;
  const version = ++playbackVersion; playing = true; $("play").textContent = "Ⅱ Pause";
  try {
    if (position === queue.length - 1) position = 0;
    message(`Playing ${queue.length} selected frames.`, "busy"); let due = performance.now();
    const tick = async () => {
      if (version !== playbackVersion) return;
      // Request the current frame first, then buffer just two ahead. Preview
      // requests decode on demand; playback never waits for the whole queue.
      const shown = show(queue[position]);
      for (let n = 1; n <= 2 && position + n < queue.length; n++) loadImage(queue[position + n]).catch(() => {});
      await shown; if (version !== playbackVersion) return;
      updatePosition();
      const duration = Math.max(0.01, queue[position].duration_seconds) / Number($("speed").value);
      due = Math.max(due, performance.now()) + duration * 1000;
      setTimeout(() => {
        if (version !== playbackVersion) return;
        if (position + 1 >= queue.length) { stop(); message("Selection finished."); return; }
        position++; tick().catch(error => { if (version === playbackVersion) { stop(); message(error.message, "error"); } });
      }, Math.max(0, due - performance.now()));
    }; await tick();
  } catch (error) { if (version === playbackVersion) { stop(); message(error.message, "error"); } }
}
function move(next) { stop(); if (!queue.length) return; position = Math.max(0, Math.min(queue.length - 1, next)); updatePosition(); show(queue[position]).catch(e => message(e.message, "error")); }
const resultTabs = ["table", "graph", "player"];
for (const [index, view] of resultTabs.entries()) {
  const tab = $("view-" + view);
  tab.addEventListener("click", () => setResultView(view));
  tab.addEventListener("keydown", event => {
    let next;
    if (event.key === "ArrowRight") next = (index + 1) % resultTabs.length;
    else if (event.key === "ArrowLeft") next = (index + resultTabs.length - 1) % resultTabs.length;
    else if (event.key === "Home") next = 0;
    else if (event.key === "End") next = resultTabs.length - 1;
    else return;
    event.preventDefault(); setResultView(resultTabs[next]); $("view-" + resultTabs[next]).focus();
  });
}
$("show-masks").addEventListener("change", maskVisibility);
$("mask-opacity").addEventListener("input", maskVisibility);
$("mask-column").addEventListener("change", () => { try { drawMasks(); } catch (error) { message(error.message, "error"); } });
for (const id of ["graph-x", "graph-y"]) $(id).addEventListener("change", () => { graphAxesChosen = true; renderGraph(); });
$("run").addEventListener("click", runQuery);
$("cancel-query").addEventListener("click", () => { if (queryController) queryController.abort(); });
$("sql").addEventListener("keydown", e => { if ((e.ctrlKey || e.metaKey) && e.key === "Enter") { e.preventDefault(); runQuery(); } });
document.querySelectorAll("[data-example]").forEach(button => button.addEventListener("click", () => { const source = $("sql").value.match(/\bFROM\s+(?:frames|tracks|files|boxes)\s*\(\s*('(?:[^']|'')*')\s*\)/i)?.[1] || "'filename.mp4'"; $("sql").value = examples[button.dataset.example].replace("'filename.mp4'", source); $("sql").style.height = Math.max(130, $("sql").value.split("\n").length * 25 + 32) + "px"; $("sql").focus(); }));
$("play").addEventListener("click", play); $("previous").addEventListener("click", () => move(position - 1)); $("next").addEventListener("click", () => move(position + 1)); $("scrub").addEventListener("input", () => move(Number($("scrub").value)));
$("order").addEventListener("change", () => { updateQueue(); if (queue.length) move(0); });
refreshInfo().catch(e => message(e.message, "error"));
