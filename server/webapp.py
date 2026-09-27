#!.venv/bin/python
"""Web front-end and archive for battery test runs.

Testers post their finished run zips here; the server stores each one under the
battery's ID, applies the same pass/fail checks as `plot_results.py <zip>`, and
serves them back so a battery's history can be browsed in a page: pick the
battery, pick the run, see the summary image, download the raw zip.

The page itself is templates/index.html, with static/style.css and static/app.js;
this module serves it and the JSON API behind it.

Endpoints:
  GET  /                                  browse the stored runs, or upload a zip by hand
  POST /runs        (form "zipfile")      store a run and return its JSON verdict
  POST /upload      (form "zipfile")      the page's form: stores the run, then shows it
  POST /check       (form "zipfile")      one-off: JSON {"overall": "PASS"/"FAIL", ...}, nothing stored
  GET  /api/batteries                     battery IDs that have runs stored
  GET  /api/batteries/<id>/runs           the runs stored for one battery
  GET  /api/runs/<id>/<run>               one run's stored verdict and metadata
  GET  /runs/<id>/<run>/summary.png       that run's summary image
  GET  /runs/<id>/<run>/zip               that run's raw zip

Usage:
    ./webapp.py                    # serves on http://127.0.0.1:5000
    ./webapp.py --port 8080
    ./webapp.py --host 0.0.0.0     # reachable from other machines on the LAN
    ./webapp.py --runs-dir ./runs  # where stored runs live (default: $BATTERY_RUNS_DIR)
"""

import argparse
import io
import json
import os
import re
import zipfile
from datetime import datetime, timezone

import matplotlib

matplotlib.use("Agg") # no display in a server process; must precede plot_results' pyplot import

import plot_results
from flask import Flask, abort, jsonify, redirect, render_template, request, send_file

app = Flask(__name__)
app.config["MAX_CONTENT_LENGTH"] = 10 * 1024 * 1024 # 10 MB, generous for a run zip

# Where stored runs live: runs/<battery id>/<run name>.{zip,json,png}, where a
# run name is RUN_NAME_SHAPE below.
RUNS_DIR = os.environ.get("BATTERY_RUNS_DIR", "/var/lib/battery-summary/runs")

# Run names come in from testers over the network and are used as filenames, so
# they're held to the shape the tester produces:
# Battery_126_Tester_bt-6329_Time_2026-09-08_09-27-56.
#
# The tester part is optional and the underscores before Time are counted loosely
# so that the older Battery_126___Time_... names, which is what everything stored
# before testers reported themselves looks like, still read back.
RUN_NAME_RE = re.compile(
    r"^Battery_(\d+)(?:_Tester_(?P<tester>[A-Za-z0-9.-]+))?_+Time_"
    r"(?P<time>\d{4}-\d{2}-\d{2}_\d{2}-\d{2}-\d{2})$")

RUN_NAME_SHAPE = "Battery_<id>_Tester_<name>_Time_<YYYY-MM-DD_HH-MM-SS>"

# Anything a tester name must not carry into a run name, which is a filename.
SAFE_TESTER_RE = re.compile(r"[^A-Za-z0-9.-]+")

NO_CSVS_FOUND = ("No full_charge_*.csv, full_discharge_*.csv or "
                 "monitoring_*.csv found in that zip.")


def error_page(message):
    """The browse page again, with what went wrong shown under the upload form."""
    return render_template("index.html", error=message), 400


def extract_dfs(zf):
    """Pull the charge/discharge/monitor CSVs out of the zip, matching
    plot_results.process_zip()'s ZIP_TARGETS matching.
    """
    names = [n for n in zf.namelist() if n.lower().endswith(".csv")]
    dfs, titles = {}, {}
    for needle, profile in plot_results.ZIP_TARGETS:
        matches = [n for n in names if needle in os.path.basename(n)]
        if not matches:
            continue
        with zf.open(matches[0]) as f:
            df = plot_results.load_df(f)
        if df is None:
            continue
        dfs[profile] = df
        titles[profile] = f"{profile.capitalize()}: {os.path.basename(matches[0])}"
    return dfs, titles


def get_uploaded_zip():
    """Pull the "zipfile" form field out of the request and open it.

    Returns (zipfile.ZipFile, filename, raw bytes, None) on success, or
    (None, None, None, error message) if the upload was missing/invalid.
    """
    uploaded = request.files.get("zipfile")
    if uploaded is None or uploaded.filename == "":
        return None, None, None, "Choose a zip file first."
    if not uploaded.filename.lower().endswith(".zip"):
        return None, None, None, "That's not a .zip file."
    raw = uploaded.read()
    try:
        zf = zipfile.ZipFile(io.BytesIO(raw))
    except zipfile.BadZipFile:
        return None, None, None, "Couldn't read that as a zip file."
    return zf, uploaded.filename, raw, None


# How to compute each profile's checks from its dataframe: (capacity/extra
# value function, checks function taking (df, t, value)).
PROFILE_CHECKERS = {
    "charge": (plot_results.charged_mAh, plot_results.charge_checks),
    "discharge": (plot_results.discharged_mAh, plot_results.discharge_checks),
    "monitor": (plot_results.monitor_pack_drift_mV,
               lambda df, t, delta_mV: plot_results.monitor_checks(delta_mV)),
}


def verdict_for_dfs(dfs, run_name):
    """Run every profile's checks over the CSVs found in a run."""
    profiles = {}
    for profile, df in dfs.items():
        t = df["timestamp"]
        value_fn, checks_fn = PROFILE_CHECKERS[profile]
        checks = checks_fn(df, t, value_fn(df, t))
        profiles[profile] = {
            "overall": "PASS" if all(c.passed for c in checks) else "FAIL",
            "checks": [{"name": c.name, "passed": bool(c.passed),
                       "measured": c.measured, "limit": c.limit}
                      for c in checks],
        }

    missing = [profile for _, profile in plot_results.ZIP_TARGETS if profile not in dfs]
    overall = "PASS" if all(p["overall"] == "PASS" for p in profiles.values()) else "FAIL"
    return {"run": run_name, "overall": overall, "profiles": profiles, "missing": missing}


# ---------------------------------------------------------------- storage ----

def battery_dir(battery_id):
    # Absolute: send_file resolves a relative path against Flask's root, not the
    # working directory, which would quietly look in the wrong place.
    return os.path.abspath(os.path.join(RUNS_DIR, str(battery_id)))


def run_paths(battery_id, run_name):
    """The zip, metadata and cached summary image for one stored run."""
    base = os.path.join(battery_dir(battery_id), run_name)
    return base + ".zip", base + ".json", base + ".png"


def valid_run_name(run_name):
    return bool(RUN_NAME_RE.match(run_name))


def valid_battery_id(battery_id):
    return str(battery_id).isdigit()


def checked_run(battery_id, run_name):
    """Validate a battery/run pair from a URL and confirm the run is stored.

    Names from a URL end up as filesystem paths, so anything that isn't the
    tester's own run-name shape is refused outright rather than sanitised.
    """
    if not valid_battery_id(battery_id) or not valid_run_name(run_name):
        abort(404)
    zip_path, meta_path, png_path = run_paths(battery_id, run_name)
    if not os.path.exists(zip_path):
        abort(404)
    return zip_path, meta_path, png_path


def read_results_json(zf):
    """The tester's own results.json from inside the zip, if it wrote one."""
    for name in zf.namelist():
        if os.path.basename(name) == "results.json":
            try:
                with zf.open(name) as f:
                    return json.load(f)
            except (json.JSONDecodeError, KeyError):
                return None
    return None


def parse_timestamp(value):
    """A results.json timestamp, however it was written, as YYYY-MM-DD_HH-MM-SS.

    The tester writes RFC 3339; the compact form is what run names use.
    """
    if not isinstance(value, str) or not value.strip():
        return None
    text = value.strip()
    try:
        return datetime.fromisoformat(text.replace("Z", "+00:00")).strftime("%Y-%m-%d_%H-%M-%S")
    except ValueError:
        pass
    try:
        return datetime.strptime(text, "%Y-%m-%d_%H-%M-%S").strftime("%Y-%m-%d_%H-%M-%S")
    except ValueError:
        return None


def run_identity(zip_name, results):
    """Who a run belongs to, when it ran, and what to file it as.

    results.json is the source of truth -- the tester writes the battery, its
    own name and the start time in there, so a zip can be called anything at
    all and still be filed correctly. The zip's top-level folder name is only
    a fallback, for runs from testers that predate those fields and for the
    ones already in the archive.

    Returns (battery_id, tester, run_name) or (None, None, why not).
    """
    results = results or {}
    from_name = RUN_NAME_RE.match(zip_name)

    # results.json first, then the form fields, then the name. The run's own
    # record of which battery and which rig outranks whoever is posting it: a
    # zip re-sent from another machine, or by hand from a laptop, would
    # otherwise be filed under the sender rather than the tester that ran it.
    given = request.form.get("battery_id", "").strip()
    battery_id = None
    for candidate in (results.get("batteryID"), given,
                      from_name.group(1) if from_name else None):
        if valid_battery_id(candidate if candidate is not None else ""):
            battery_id = int(candidate)
            break

    tester = str(  # "tester" is what testers before the README's schema wrote.
        results.get("testerName") or results.get("tester")
        or request.form.get("tester", "").strip()
        or (from_name.group("tester") if from_name else "") or "")
    tester = SAFE_TESTER_RE.sub("-", tester).strip("-")

    if battery_id is None:
        return None, None, ("no battery ID: results.json has no batteryID, and "
                            f"{zip_name!r} isn't of the form {RUN_NAME_SHAPE}")

    stamp = parse_timestamp(results.get("timestamp"))
    if stamp is None and from_name:
        stamp = from_name.group("time")
    if stamp is None:
        return None, None, ("no start time: results.json has no usable timestamp, and "
                            f"{zip_name!r} isn't of the form {RUN_NAME_SHAPE}")

    # A zip that already arrived named for the battery, tester and time it was
    # filed under keeps that name: rebuilding it would file a second copy of a
    # run already in the archive under a slightly different spelling, which is
    # what would happen to every older Battery_<id>___Time_<time> run.
    if (from_name and str(battery_id) == from_name.group(1)
            and tester == (from_name.group("tester") or "")
            and stamp == from_name.group("time")):
        return battery_id, tester, zip_name

    # Otherwise the name is built from what the run says it is, so two uploads
    # of the same run land on the same file however the zips were named -- and
    # a run never ends up filed under one battery with another in its name.
    run_name = (f"Battery_{battery_id}_Tester_{tester}_Time_{stamp}" if tester
                else f"Battery_{battery_id}_Time_{stamp}")
    return battery_id, tester, run_name


def store_run(raw, run_name, battery_id, tester, results, verdict):
    """Write the zip and its metadata into the archive, replacing any run of the
    same name (a tester re-offers a run it isn't sure the server took).
    """
    os.makedirs(battery_dir(battery_id), exist_ok=True)
    zip_path, meta_path, png_path = run_paths(battery_id, run_name)

    with open(zip_path, "wb") as f:
        f.write(raw)

    meta = dict(verdict)
    meta.update({
        "run": run_name,
        "battery_id": battery_id,
        "tester": tester,
        "uploaded": datetime.now(timezone.utc).isoformat(timespec="seconds"),
        "time": run_time(run_name),
        "size": len(raw),
        # Kept as the tester wrote it: this is the run's own account of itself,
        # and the page shows it as it stands.
        "results": results,
    })
    with open(meta_path, "w") as f:
        json.dump(meta, f, indent=2)

    # A stale image from a replaced run would otherwise be served forever.
    if os.path.exists(png_path):
        os.remove(png_path)
    return meta


def render_summary(dfs, titles, png_path):
    """Draw a run's combined summary image to png_path."""
    # Two workers can be asked for the same new run at once, so the image is
    # rendered aside and moved into place, never served half-written. The .png
    # suffix stays on the temp name: matplotlib picks the format from it.
    tmp_path = f"{png_path}.{os.getpid()}.tmp.png"
    plot_results.plot_combined(dfs, titles, tmp_path)
    os.replace(tmp_path, png_path)


def run_time(run_name):
    """The run's timestamp, pulled out of its name for display and sorting."""
    m = RUN_NAME_RE.match(run_name)
    if not m:
        return run_name
    try:
        return datetime.strptime(m.group("time"), "%Y-%m-%d_%H-%M-%S").isoformat(
            sep=" ", timespec="seconds")
    except ValueError:
        return m.group("time")


def run_tester(run_name):
    """The tester named in the run's name, or "" for a run from before they were."""
    m = RUN_NAME_RE.match(run_name)
    return (m.group("tester") or "") if m else ""


def stored_runs(battery_id):
    """Every run stored for one battery, newest first.

    One unreadable run -- bad JSON, or a file the service user can't open
    because it was written by hand as someone else -- must not take the whole
    listing down with it, so anything that won't read is logged and skipped.
    """
    directory = battery_dir(battery_id)
    if not os.path.isdir(directory):
        return []
    runs = []
    for name in sorted(os.listdir(directory)):
        if not name.endswith(".zip"):
            continue
        run_name = name[: -len(".zip")]
        meta_path = os.path.join(directory, run_name + ".json")
        meta = {}
        if os.path.exists(meta_path):
            try:
                with open(meta_path) as f:
                    meta = json.load(f)
            except (json.JSONDecodeError, OSError) as e:
                app.logger.warning("Reading %s: %s", meta_path, e)
                meta = {}
        # "completed" is the README's field; "passed" is what testers wrote before it.
        results = meta.get("results") or {}
        passed = results.get("completed", results.get("passed"))
        runs.append({
            "run": run_name,
            "time": meta.get("time") or run_time(run_name),
            "overall": meta.get("overall", "?"),
            "testerOverall": "" if passed is None else ("PASS" if passed else "FAIL"),
            "tester": meta.get("tester") or run_tester(run_name),
            "size": meta.get("size") or file_size(os.path.join(directory, name)),
        })
    # Sorted on the timestamp, not the name: with the tester sitting between the
    # battery and the time, names no longer sort into time order, and two testers'
    # runs would interleave by hostname.
    return sorted(runs, key=lambda r: (r["time"], r["run"]), reverse=True)


def file_size(path):
    try:
        return os.path.getsize(path)
    except OSError:
        return 0


def archive_problem():
    """Why the archive can't be listed, if it can't -- an empty page with no
    explanation is the worst way to find out the server is looking elsewhere.
    """
    if not os.path.isdir(RUNS_DIR):
        return f"{RUNS_DIR} doesn't exist yet -- nothing has been uploaded, or BATTERY_RUNS_DIR points somewhere else."
    if not os.access(RUNS_DIR, os.R_OK | os.X_OK):
        return f"{RUNS_DIR} can't be read by the user this app runs as."
    return None


def stored_batteries():
    """Every battery with runs stored, lowest ID first."""
    if archive_problem():
        return []
    batteries = []
    for name in sorted(os.listdir(RUNS_DIR), key=lambda n: int(n) if n.isdigit() else 0):
        if not valid_battery_id(name):
            continue
        try:
            runs = stored_runs(name)
        except OSError as e:
            app.logger.warning("Listing battery %s: %s", name, e)
            continue
        if not runs:
            continue
        batteries.append({
            "id": int(name),
            "runs": len(runs),
            "latest": runs[0]["run"],
            "latestOverall": runs[0]["overall"],
        })
    return batteries


# ----------------------------------------------------------------- routes ----

@app.get("/")
def index():
    # Never let a browser hold on to an older copy of the page: its JavaScript is
    # what talks to the API, so a stale one looks like a broken server.
    return render_template("index.html"), 200, {"Cache-Control": "no-store"}


@app.post("/upload")
def upload():
    """The page's upload form: store the run, then show it in the browser.

    This files the run exactly as a tester's POST to /runs does -- a zip
    uploaded here is one you can come back to later.
    """
    zf, filename, raw, err = get_uploaded_zip()
    if err:
        return error_page(err)

    with zf:
        results = read_results_json(zf)
        stored, err = keep_run(raw, zf, plot_results.zip_run_name(zf, filename), results)
        if err:
            return error_page(err)

    return redirect(f"/?battery={stored['battery_id']}&run={stored['run']}")


@app.post("/check")
def check():
    zf, filename, _, err = get_uploaded_zip()
    if err:
        return jsonify(error=err), 400

    with zf:
        dfs, _ = extract_dfs(zf)
        if not dfs:
            return jsonify(error=NO_CSVS_FOUND), 400
        run_name = plot_results.zip_run_name(zf, filename)

    return jsonify(verdict_for_dfs(dfs, run_name))


def keep_run(raw, zf, zip_name, results):
    """File a run under its battery and check it, whoever sent it.

    Returns (metadata, None), or (None, why it couldn't be filed).
    """
    battery_id, tester, run_name = run_identity(zip_name, results)
    if battery_id is None:
        # run_name carries the reason when the identity couldn't be worked out.
        return None, f"Couldn't file this run: {run_name}."

    dfs, titles = extract_dfs(zf)
    if not dfs:
        # Still worth keeping: a run that failed early has no full CSVs but
        # its readings are exactly what someone will want to look at.
        verdict = {"run": run_name, "overall": "FAIL", "profiles": {},
                   "missing": [p for _, p in plot_results.ZIP_TARGETS]}
    else:
        verdict = verdict_for_dfs(dfs, run_name)


    meta = store_run(raw, run_name, battery_id, tester, results, verdict)

    # Drawn now rather than on first view: a stored run is complete on disk, and
    # opening it doesn't wait on matplotlib.
    if dfs:
        _, _, png_path = run_paths(battery_id, run_name)
        try:
            render_summary(dfs, titles, png_path)
        except Exception as e:
            # The run itself is safely stored; the image can be drawn later.
            app.logger.warning("Rendering %s: %s", png_path, e)
    return meta, None


@app.post("/runs")
def store():
    """Take a run zip from a tester: store it under its battery, check it, and
    hand back the verdict.
    """
    zf, filename, raw, err = get_uploaded_zip()
    if err:
        return jsonify(error=err), 400

    with zf:
        results = read_results_json(zf)
        meta, err = keep_run(raw, zf, plot_results.zip_run_name(zf, filename), results)
        if err:
            return jsonify(error=err), 400

    return jsonify(meta)


@app.get("/api/batteries")
def api_batteries():
    batteries = stored_batteries()
    return jsonify(batteries=batteries,
                   archive=RUNS_DIR,
                   runs=sum(b["runs"] for b in batteries),
                   problem=archive_problem())


@app.get("/api/batteries/<battery_id>/runs")
def api_battery_runs(battery_id):
    if not valid_battery_id(battery_id):
        abort(404)
    return jsonify(battery=int(battery_id), runs=stored_runs(battery_id))


@app.get("/api/runs/<battery_id>/<run_name>")
def api_run(battery_id, run_name):
    _, meta_path, _ = checked_run(battery_id, run_name)
    if not os.path.exists(meta_path):
        abort(404)
    with open(meta_path) as f:
        return jsonify(json.load(f))


@app.get("/runs/<battery_id>/<run_name>/zip")
def run_zip(battery_id, run_name):
    zip_path, _, _ = checked_run(battery_id, run_name)
    return send_file(zip_path, mimetype="application/zip", as_attachment=True,
                     download_name=f"{run_name}.zip")


@app.get("/runs/<battery_id>/<run_name>/summary.png")
def run_summary(battery_id, run_name):
    zip_path, _, png_path = checked_run(battery_id, run_name)

    # Runs stored before the image was drawn at upload time, and any whose render
    # failed, are drawn on demand here.
    if not os.path.exists(png_path):
        with zipfile.ZipFile(zip_path) as zf:
            dfs, titles = extract_dfs(zf)
        if not dfs:
            abort(404)
        render_summary(dfs, titles, png_path)

    return send_file(png_path, mimetype="image/png")


def main():
    global RUNS_DIR
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--host", default="127.0.0.1",
                    help="default: 127.0.0.1 (localhost only); use 0.0.0.0 for LAN access")
    ap.add_argument("--port", type=int, default=5000)
    ap.add_argument("--runs-dir", default=RUNS_DIR,
                    help=f"where stored runs are kept (default: {RUNS_DIR})")
    ap.add_argument("--debug", action="store_true")
    args = ap.parse_args()

    RUNS_DIR = args.runs_dir
    app.run(host=args.host, port=args.port, debug=args.debug)


if __name__ == "__main__":
    main()
