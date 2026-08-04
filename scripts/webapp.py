#!.venv/bin/python
"""Web front-end for plot_results.py: upload a run zip, get results back.

Runs the same charge/discharge/monitor extraction and pass/fail checks as
`plot_results.py <zip>`, but over HTTP instead of the command line.

Two ways to use it:
  POST /upload  (form field "zipfile") -> combined 1920x1080 summary.png download
  POST /check   (form field "zipfile") -> JSON {"overall": "PASS"/"FAIL", ...}

The /check endpoint is meant for other programs (e.g. a CI step hitting it
with Postman or curl) that just want a pass/fail verdict without the image.

Usage:
    ./webapp.py                    # serves on http://127.0.0.1:5000
    ./webapp.py --port 8080
    ./webapp.py --host 0.0.0.0     # reachable from other machines on the LAN
"""

import argparse
import io
import os
import zipfile

import matplotlib
matplotlib.use("Agg") # no display in a server process; must precede plot_results' pyplot import

from flask import Flask, jsonify, request, send_file

import plot_results

app = Flask(__name__)
app.config["MAX_CONTENT_LENGTH"] = 100 * 1024 * 1024 # 100 MB, generous for a run zip

UPLOAD_FORM = """<!doctype html>
<title>Battery run summary</title>
<style>
  body { font-family: system-ui, sans-serif; max-width: 34rem; margin: 4rem auto; padding: 0 1rem; }
  h1 { font-size: 1.3rem; }
  .drop { border: 2px dashed #999; border-radius: 8px; padding: 2.5rem 1rem; text-align: center; }
  .error { color: #b00020; margin-top: 1rem; }
  button { font-size: 1rem; padding: 0.5rem 1.2rem; margin-top: 1rem; cursor: pointer; }
</style>
<h1>Battery run summary</h1>
<p>Upload a run zip (containing full_charge_*.csv, full_discharge_*.csv and/or
monitoring_*.csv) to get the combined charge/discharge/monitor summary image.</p>
<form method="post" action="/upload" enctype="multipart/form-data">
  <div class="drop">
    <input type="file" name="zipfile" accept=".zip" required>
    <br><button type="submit">Upload and process</button>
  </div>
</form>
<p>API: <code>POST /check</code> with the same zip as form field
<code>zipfile</code> returns a JSON PASS/FAIL verdict instead of an image.</p>
__ERROR__
"""

NO_CSVS_FOUND = ("No full_charge_*.csv, full_discharge_*.csv or "
                 "monitoring_*.csv found in that zip.")


def error_page(message):
    html = UPLOAD_FORM.replace("__ERROR__", f'<p class="error">{message}</p>')
    return html, 400


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

    Returns (zipfile.ZipFile, filename, None) on success, or
    (None, None, error message) if the upload was missing/invalid.
    """
    uploaded = request.files.get("zipfile")
    if uploaded is None or uploaded.filename == "":
        return None, None, "Choose a zip file first."
    if not uploaded.filename.lower().endswith(".zip"):
        return None, None, "That's not a .zip file."
    try:
        zf = zipfile.ZipFile(io.BytesIO(uploaded.read()))
    except zipfile.BadZipFile:
        return None, None, "Couldn't read that as a zip file."
    return zf, uploaded.filename, None


@app.get("/")
def index():
    return UPLOAD_FORM.replace("__ERROR__", "")


@app.post("/upload")
def upload():
    zf, filename, err = get_uploaded_zip()
    if err:
        return error_page(err)

    with zf:
        dfs, titles = extract_dfs(zf)
        if not dfs:
            return error_page(NO_CSVS_FOUND)
        run_name = plot_results.zip_run_name(zf, filename)

    png = io.BytesIO()
    plot_results.plot_combined(dfs, titles, png)
    png.seek(0)

    return send_file(png, mimetype="image/png", as_attachment=True,
                     download_name=f"{run_name}_summary.png")


# How to compute each profile's checks from its dataframe: (capacity/extra
# value function, checks function taking (df, t, value)).
PROFILE_CHECKERS = {
    "charge": (plot_results.charged_mAh, plot_results.charge_checks),
    "discharge": (plot_results.discharged_mAh, plot_results.discharge_checks),
    "monitor": (plot_results.monitor_pack_drift_mV,
               lambda df, t, delta_mV: plot_results.monitor_checks(delta_mV)),
}


@app.post("/check")
def check():
    zf, filename, err = get_uploaded_zip()
    if err:
        return jsonify(error=err), 400

    with zf:
        dfs, titles = extract_dfs(zf)
        if not dfs:
            return jsonify(error=NO_CSVS_FOUND), 400
        run_name = plot_results.zip_run_name(zf, filename)

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

    return jsonify(run=run_name, overall=overall, profiles=profiles, missing=missing)


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--host", default="127.0.0.1",
                    help="default: 127.0.0.1 (localhost only); use 0.0.0.0 for LAN access")
    ap.add_argument("--port", type=int, default=5000)
    ap.add_argument("--debug", action="store_true")
    args = ap.parse_args()

    app.run(host=args.host, port=args.port, debug=args.debug)


if __name__ == "__main__":
    main()
