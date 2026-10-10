"use strict";
const $ = id => document.getElementById(id);
const examples = {
	 motion: "SELECT frame, pts_seconds,\n  motion(frame, LAG(frame) OVER (\n    PARTITION BY path, track_id\n    ORDER BY pts_seconds, sample_index\n  )) AS motion_score\nFROM frames\nORDER BY path, track_id, pts_seconds\nLIMIT 100",
  mixed: "SELECT frame, path, sample_index, pts_seconds\nFROM frames\nLIMIT 48",
  keyframes: "SELECT frame, path, pts_seconds, size\nFROM frames\nWHERE is_keyframe = true\nLIMIT 100",
  tracks: "SELECT path, track_id, handler, codec, width, height\nFROM tracks"
};
let result = { columns: [], rows: [] }, selection = new Set(), queue = [], position = 0;
let generation = 0, previewVersion = 0, playbackVersion = 0, playing = false, decoderAvailable = false;
const ready = new Set(), images = new Map();
let queryController = null;
const frameValues = row => Object.values(row).filter(v => v && typeof v === "object" && v.type === "frame" && v.id);
function message(text, kind = "") { $("message").textContent = text; $("message").className = "message " + kind; }
function text(tag, value, className) { const el = document.createElement(tag); el.textContent = value; if (className) el.className = className; return el; }
async function api(path, body, method) {
  const response = await fetch(path, body ? { method: method || "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) } : method ? { method } : {});
  const data = await response.json(); if (!response.ok) throw new Error(data.error || "Request failed"); return data;
}
async function refreshInfo() {
  const info = await api("/api/info"); decoderAvailable = info.decoder_available;
  $("decoder-note").hidden = decoderAvailable; $("sources").replaceChildren();
  for (const src of info.sources) {
    const item = text("div", "", "source"), remove = text("button", "×", "remove-source");
    const name = text("span", src.name, "source-name");
    name.title = `${src.name} · ${src.frames == null ? "Ready to query" : src.frames.toLocaleString() + " frames"} · ${(src.size / 1048576).toFixed(1)} MB`;
    remove.title = `Remove ${src.name} from this session`; remove.setAttribute("aria-label", `Remove ${src.name}`);
    remove.addEventListener("click", () => removeSource(src.id, remove));
    item.append(name, remove); $("sources").append(item);
  }
  return info;
}
async function removeSource(id, button) {
  if (queryController) queryController.abort(); queryController = null;
  generation++; previewVersion++; stop(); $("run").disabled = false; $("cancel-query").hidden = true; button.disabled = true;
  try {
    await api("/api/source/" + id, null, "DELETE"); const info = await refreshInfo();
    if (info.sources.length) await runQuery();
    else {
      result = { columns: [], rows: [] }; selection.clear(); renderTable(); updateQueue();
      $("preview").hidden = true; $("preview-empty").hidden = false; $("frame-badge").hidden = true; $("download").hidden = true;
      $("frame-title").textContent = "No frame selected"; $("frame-detail").textContent = "Binary frame + decoded preview"; $("result-note").textContent = "Open a video to query.";
      $("run").disabled = false; message("Source removed from the session. Open a video to continue.");
    }
  } catch (error) { button.disabled = false; message(error.message, "error"); }
}
function stop() { playing = false; playbackVersion++; $("play").textContent = "▶ Play selection"; }
function updateQueue(stopPlayback = true) {
  if (stopPlayback) stop(); const unique = new Map();
  result.rows.forEach((row, index) => { if (selection.has(index)) for (const f of frameValues(row)) unique.set(f.id, f); });
  queue = [...unique.values()];
  if ($("order").value === "presentation") {
    const groups = new Map(); for (const f of queue) { const key = f.id.split("-")[0] + ":" + f.track_id; if (!groups.has(key)) groups.set(key, groups.size); }
    const group = f => groups.get(f.id.split("-")[0] + ":" + f.track_id);
    queue.sort((a, b) => group(a) - group(b) || a.pts_seconds - b.pts_seconds || a.sample_index - b.sample_index);
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
        const button = text("button", `▣ Frame ${value.sample_index}`, "frame-cell"); button.title = `${value.size} encoded bytes · preview frame`; button.dataset.frame = value.id;
        button.addEventListener("click", () => { stop(); const p = queue.findIndex(f => f.id === value.id); if (p >= 0) { position = p; updatePosition(); } show(value).catch(e => message(e.message, "error")); }); td.append(button);
      } else if (value == null) td.append(text("span", "null", "null"));
      else td.textContent = typeof value === "number" && !Number.isInteger(value) ? Number(value.toFixed(6)).toString() : String(value);
      tr.append(td);
    } body.append(tr);
  });
  function allState() { const all = head.querySelector("input"); if (!all) return; const count = result.rows.filter(r => frameValues(r).length).length; all.checked = selection.size === count; all.indeterminate = selection.size > 0 && selection.size < count; }
}
async function prepare(frames) {
  const ids = frames.filter(f => !ready.has(f.id)).map(f => f.id); if (!ids.length) return;
  await api("/api/render", { ids }); ids.forEach(id => ready.add(id));
}
function loadImage(frame) {
  if (images.has(frame.id)) return images.get(frame.id);
  const promise = new Promise((resolve, reject) => { const img = new Image(); img.onload = () => resolve(img); img.onerror = () => reject(new Error("Could not load frame preview")); img.src = frame.preview_url; });
  images.set(frame.id, promise); if (images.size > 8) images.delete(images.keys().next().value);
  promise.catch(() => images.delete(frame.id)); return promise;
}
async function show(frame, quiet = false) {
  const version = ++previewVersion;
  $("download").href = frame.raw_url; $("download").hidden = false;
  $("frame-title").textContent = `${frame.source} · Frame ${frame.sample_index}`;
  $("frame-detail").textContent = `Track ${frame.track_id} · PTS ${frame.pts_seconds.toFixed(6)} s · ${frame.size.toLocaleString()} bytes`;
  $("results").querySelectorAll("tbody tr").forEach(tr => tr.classList.toggle("active", frameValues(result.rows[Number(tr.dataset.row)]).some(f => f.id === frame.id)));
  if (!decoderAvailable) return;
  if (!ready.has(frame.id)) { if (!quiet) message(`Rendering frame ${frame.sample_index}…`, "busy"); await prepare([frame]); }
  const img = await loadImage(frame); if (version !== previewVersion) return;
  $("preview").src = img.src; $("preview").hidden = false; $("preview-empty").hidden = true;
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
  result = { columns: [], rows: [] }; selection.clear(); renderTable(); updateQueue();
  $("preview").hidden = true; $("preview-empty").hidden = false; $("frame-badge").hidden = true; $("download").hidden = true;
  $("frame-title").textContent = "No frame selected"; $("frame-detail").textContent = "Binary frame + decoded preview";
  const timing = () => firstRow == null ? "" : ` · first row in ${(firstRow / 1000).toFixed(2)} s`;
  try {
    await readQueryStream($("sql").value, controller.signal, event => {
      if (version !== generation) return;
      if (event.type === "schema") {
        result.columns = event.columns; renderTable(); $("result-note").textContent = "Preparing query input…";
      } else if (event.type === "rows") {
        const first = result.rows.length === 0;
        for (const row of event.rows) { const index = result.rows.length; result.rows.push(row); if (frameValues(row).length) selection.add(index); }
        if (firstRow == null && event.rows.length) firstRow = performance.now() - started;
        renderTable(); updateQueue(false); $("result-note").textContent = "Receiving rows…";
        message(`${result.rows.length} rows received${timing()}…`, "busy");
        if (first && queue.length) show(queue[0], true).catch(error => { if (version === generation) message(`Preview: ${error.message}`, "error"); });
      } else if (event.type === "done") {
        $("result-note").textContent = event.truncated ? "Showing 500 rows. Add LIMIT or narrow WHERE." : queue.length ? "Select rows for playback · click a frame to inspect" : "Scalar query results";
        message(`${result.rows.length} rows returned${timing()}${event.truncated ? " · result capped at 500 rows" : ""}.`);
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
    message(`Preparing ${queue.length} selected frames…`, "busy"); await prepare(queue); if (version !== playbackVersion) return;
    if (position === queue.length - 1) position = 0;
    message(`Playing ${queue.length} selected frames.`, "busy"); let due = performance.now();
    const tick = async () => {
      if (version !== playbackVersion) return;
      await show(queue[position]); if (version !== playbackVersion) return;
      updatePosition(); for (let n = 1; n <= 2 && position + n < queue.length; n++) loadImage(queue[position + n]).catch(() => {});
      const duration = Math.max(0.01, queue[position].duration_seconds) / Number($("speed").value);
      due = Math.max(due, performance.now()) + duration * 1000;
      setTimeout(() => {
        if (version !== playbackVersion) return;
        if (position + 1 >= queue.length) { stop(); message("Selection finished."); return; }
        position++; tick().catch(error => { stop(); message(error.message, "error"); });
      }, Math.max(0, due - performance.now()));
    }; await tick();
  } catch (error) { if (version === playbackVersion) { stop(); message(error.message, "error"); } }
}
function move(next) { stop(); if (!queue.length) return; position = Math.max(0, Math.min(queue.length - 1, next)); updatePosition(); show(queue[position]).catch(e => message(e.message, "error")); }
$("run").addEventListener("click", runQuery);
$("cancel-query").addEventListener("click", () => { if (queryController) queryController.abort(); });
$("sql").addEventListener("keydown", e => { if ((e.ctrlKey || e.metaKey) && e.key === "Enter") { e.preventDefault(); runQuery(); } });
document.querySelectorAll("[data-example]").forEach(button => button.addEventListener("click", () => { $("sql").value = examples[button.dataset.example]; $("sql").style.height = Math.max(130, $("sql").value.split("\n").length * 25 + 32) + "px"; $("sql").focus(); }));
$("play").addEventListener("click", play); $("previous").addEventListener("click", () => move(position - 1)); $("next").addEventListener("click", () => move(position + 1)); $("scrub").addEventListener("input", () => move(Number($("scrub").value)));
$("order").addEventListener("change", () => { updateQueue(); if (queue.length) move(0); });
$("upload").addEventListener("change", async () => {
  stop(); const files = [...$("upload").files]; $("upload").disabled = true; $("run").disabled = true;
  try {
    for (const file of files) {
      message(`Loading ${file.name}…`, "busy"); const body = new FormData(); body.append("file", file);
      const response = await fetch("/api/upload", { method: "POST", body }); const data = await response.json(); if (!response.ok) throw new Error(data.error || "Upload failed");
    }
    await refreshInfo(); message("Video registered. Run a query to read its metadata.");
  } catch (error) { await refreshInfo().catch(() => {}); message(error.message, "error"); }
  finally { $("upload").value = ""; $("upload").disabled = false; $("run").disabled = false; }
});
$("local-source").addEventListener("submit", async e => {
  e.preventDefault(); const button = $("open-path"); button.disabled = true;
  try {
    await api("/api/source", { path: $("local-path").value }); await refreshInfo();
    $("local-path").value = ""; $("path-picker").open = false; message("Video registered in place. Run a query to read its metadata.");
  } catch (error) { message(error.message, "error"); }
  finally { button.disabled = false; }
});
refreshInfo().catch(e => message(e.message, "error"));
