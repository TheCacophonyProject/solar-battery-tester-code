const batterySel = document.getElementById("battery");
const batteryFilter = document.getElementById("batteryFilter");
const runSel = document.getElementById("run");
const detail = document.getElementById("detail");
const summary = document.getElementById("summary");
const results = document.getElementById("results");
const footer = document.getElementById("footer");
const actions = document.getElementById("actions");

function verdictHTML(overall) {
  return `<span class="verdict ${overall}">${overall}</span>`;
}

let batteries = [];

function fail(message) {
  batterySel.innerHTML = '<option value="">Unavailable</option>';
  detail.innerHTML = `<span class="error">${message}</span>`;
}

// The list is filtered here rather than in a request: a workshop's worth of
// batteries is a small list, and typing shouldn't wait on the network.
function renderBatteries() {
  const needle = batteryFilter.value.trim().toLowerCase();
  const shown = needle
    ? batteries.filter(b => String(b.id).includes(needle))
    : batteries;
  if (!shown.length) {
    batterySel.innerHTML = `<option value="">${batteries.length ? "No matching battery" : "No runs stored yet"}</option>`;
    return;
  }
  const chosen = batterySel.value;
  batterySel.innerHTML = '<option value="">Pick a battery…</option>' +
    shown.map(b =>
      `<option value="${b.id}">Battery ${b.id} — ${b.runs} run${b.runs === 1 ? "" : "s"}, ` +
      `latest ${b.latestOverall || "?"}</option>`).join("");
  // Keep the picked battery selected when filtering doesn't drop it.
  if (shown.some(b => String(b.id) === chosen)) batterySel.value = chosen;
}

async function loadBatteries() {
  let data;
  try {
    const res = await fetch("/api/batteries");
    if (!res.ok) throw new Error(`the server answered ${res.status} ${res.statusText}`);
    data = await res.json();
  } catch (e) {
    fail(`Couldn't load the stored runs: ${e.message}.`);
    return;
  }
  batteries = data.batteries || [];
  renderBatteries();
  // Says where the server is actually looking, so "my upload isn't here" can be
  // answered without going and reading the unit file.
  footer.textContent = data.problem
    ? data.problem
    : `${batteries.length} batter${batteries.length === 1 ? "y" : "ies"}, ` +
      `${data.runs} run${data.runs === 1 ? "" : "s"} stored in ${data.archive}`;
  if (data.problem) footer.className = "error";
}

async function loadRuns(batteryID) {
  summary.innerHTML = "";
  actions.innerHTML = "";
  detail.textContent = "";
  results.innerHTML = "";
  if (!batteryID) {
    runSel.innerHTML = '<option value="">Pick a battery first</option>';
    runSel.disabled = true;
    return;
  }
  const res = await fetch(`/api/batteries/${batteryID}/runs`);
  const data = await res.json();
  runSel.disabled = false;
  // Newest first: the run you just did is nearly always the one you want.
  runSel.innerHTML = '<option value="">Pick a test…</option>' +
    data.runs.map(r => {
      // Both verdicts matter: the checks can pass on a run the tester itself
      // stopped part way through.
      const verdicts = r.testerOverall && r.testerOverall !== r.overall
        ? `checks ${r.overall}, tester ${r.testerOverall}` : r.overall;
      // The tester is worth seeing in the list now that runs from several rigs
      // land in the same battery.
      const on = r.tester ? ` — ${r.tester}` : "";
      return `<option value="${r.run}">${r.time} — ${verdicts}${on}</option>`;
    }).join("");
}

// How each results.json field is shown. Both spellings are here because runs
// stored before the README settled the schema used the older names.
const RESULT_FIELDS = {
  completed: ["Test completed", "yesno"],
  passed: ["Test completed", "yesno"],
  ocdPassed: ["Over current protection", "passfail"],
  ocdPass: ["Over current protection", "passfail"],
  scdPassed: ["Short circuit protection", "passfail"],
  shortCircuitPass: ["Short circuit protection", "passfail"],
  batteryID: ["Battery ID", "text"],
  testerName: ["Tester", "text"],
  tester: ["Tester", "text"],
  timestamp: ["Started", "text"],
  failureReason: ["Failed because", "text"],
};

function escapeHTML(value) {
  return String(value).replace(/[&<>"]/g, c =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
}

function resultValueHTML(value, kind) {
  if (kind === "passfail" && typeof value === "boolean") {
    return verdictHTML(value ? "PASS" : "FAIL");
  }
  if (kind === "yesno" && typeof value === "boolean") {
    return value ? "Yes" : "No";
  }
  return escapeHTML(value);
}

// The run's own account of itself, straight out of results.json. Unknown keys
// are shown too, so a field added to the tester appears here without this
// needing to know about it.
function renderResults(meta) {
  if (!meta.results || !Object.keys(meta.results).length) {
    results.innerHTML = '<p class="muted">This run has no results.json — it came ' +
      'from a tester that didn\'t write one.</p>';
    return;
  }
  const rows = Object.entries(meta.results).map(([key, value]) => {
    const [label, kind] = RESULT_FIELDS[key] || [key, "text"];
    return `<tr><th>${escapeHTML(label)}</th><td>${resultValueHTML(value, kind)}</td></tr>`;
  });
  results.innerHTML =
    `<table class="results">${rows.join("")}</table>` +
    `<details><summary>results.json</summary>` +
    `<pre>${escapeHTML(JSON.stringify(meta.results, null, 2))}</pre></details>`;
}

// The plots are drawn 1920x1080 but the page shows them at about half that, so
// a click hands the whole screen over to the image and another gives it back.
// Escape works too -- the browser handles that itself.
function toggleFullScreen(img) {
  if (document.fullscreenElement) {
    document.exitFullscreen();
    return;
  }
  if (!img.requestFullscreen) {
    // Older Safari and anything else without the API: the image's own tab is
    // the next best thing.
    window.open(img.src, "_blank");
    return;
  }
  img.requestFullscreen().catch(() => window.open(img.src, "_blank"));
}

async function showRun(batteryID, run) {
  summary.innerHTML = "";
  actions.innerHTML = "";
  detail.textContent = "";
  results.innerHTML = "";
  if (!batteryID || !run) return;

  const res = await fetch(`/api/runs/${batteryID}/${run}`);
  const meta = await res.json();

  // The checks the server ran. What the tester itself found is in the table
  // below, rather than being half-repeated here.
  const bits = [`Checks: ${verdictHTML(meta.overall)}`];
  if (meta.tester) bits.push(`ran on ${escapeHTML(meta.tester)}`);
  if (meta.missing && meta.missing.length) bits.push(`no readings for: ${meta.missing.join(", ")}`);
  detail.innerHTML = bits.join(" · ");
  renderResults(meta);

  if (Object.keys(meta.profiles || {}).length === 0) {
    summary.innerHTML = '<p class="muted">This run has no charge, discharge or monitor ' +
      'readings to plot — it stopped before they were recorded. The raw zip still has ' +
      'whatever it did record.</p>';
  } else {
    summary.innerHTML = '<p class="muted">Drawing the summary…</p>';
    const img = new Image();
    img.alt = `Summary plots for ${run}`;
    img.onload = () => { summary.innerHTML = ""; summary.appendChild(img); };
    img.onerror = () => {
      summary.innerHTML = '<p class="error">Couldn\'t draw the summary for this run. ' +
        'The raw zip is still here.</p>';
    };
    img.title = "Click to view full screen";
    img.addEventListener("click", () => toggleFullScreen(img));
    img.src = `/runs/${batteryID}/${run}/summary.png`;
  }
  actions.innerHTML =
    `<a class="button" href="/runs/${batteryID}/${run}/zip">Download raw zip</a>` +
    (Object.keys(meta.profiles || {}).length
      ? ` <a class="button" href="/runs/${batteryID}/${run}/summary.png" download>Download image</a>`
      : "");
}

batteryFilter.addEventListener("input", renderBatteries);
batterySel.addEventListener("change", () => loadRuns(batterySel.value));
runSel.addEventListener("change", () => showRun(batterySel.value, runSel.value));

// ?battery=&run= comes back from an upload: open on the run that was just
// stored instead of leaving it to be hunted for in the dropdowns.
async function start() {
  await loadBatteries();
  const params = new URLSearchParams(location.search);
  const battery = params.get("battery");
  const run = params.get("run");
  if (!battery) return;
  batterySel.value = battery;
  await loadRuns(battery);
  if (!run) return;
  runSel.value = run;
  await showRun(battery, run);
}
start();
