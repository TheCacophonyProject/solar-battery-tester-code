#!.venv/bin/python
"""Plot a solar-battery-tester run from a CSV file or a results zip.

The CSV columns are those written by makeStateCSVWriter() in hardware.go.

The plot layout depends on the run profile:
    discharge:  Temps / Discharge current / Pack voltage / Cell voltages
    charge:     Temps / Charge current (with phases) / Pack+Vbus / Cell voltages
    monitor:    Temps / Pack voltage (split at 6 h) / Cell voltages

The profile is auto-detected from the CSV filename prefix
(discharging_*, charging_*, monitoring_*) or set with --profile.

Given a zip file (as produced for a battery run, containing
full_charge_*.csv, full_discharge_*.csv and monitoring_*.csv among other
files), the full charge, full discharge and monitoring graphs are extracted,
plotted and saved to an output folder instead.

Usage:
    ./plot_results.py discharging_2026-07-02_10-30-00.csv
    ./plot_results.py charging_2026-07-02_10-30-00.csv --save charge.png
    ./plot_results.py results.csv --profile discharge
    ./plot_results.py Battery_118___Time_2026-07-30_16-09-29.zip
    ./plot_results.py results.zip --outdir plots/
"""

import argparse
import os
import sys
import zipfile
from collections import namedtuple

import matplotlib.pyplot as plt
import numpy as np
import pandas as pd

PROFILES = ("discharge", "charge", "monitor")

TEMP_COLS = ["tempAHT_C", "tempBQ76920_C", "tempBQ25798_C"]
CELL_COLS = ["cell1_mV", "cell2_mV", "cell3_mV"]

# A single pass/fail check against a run's measured value.
Check = namedtuple("Check", ["name", "passed", "measured", "limit"])

# Number of readings averaged for the smoothed trace (and the start/end
# voltages in its legend) on the monitor profile's 6 h+ pack panel.
SMOOTH_WINDOW = 600 # 10 minutes as reading every 10 seconds

# Pass/fail limits used by charge_checks(), discharge_checks() and
# monitor_checks() below. Tune these to adjust what counts as a pass.
CHARGE_MAX_TEMP_C = 50
CHARGE_MAX_DURATION_H = 10
CHARGE_FINAL_PACK_V = 12.3
CHARGE_FINAL_PACK_TOLERANCE_V = 0.1
CHARGE_MAX_CELL_V = 4.15
CHARGE_MIN_CAPACITY_MAH = 11500

DISCHARGE_MAX_TEMP_C = 50
DISCHARGE_MIN_CAPACITY_MAH = 11000
DISCHARGE_MIN_CELL_V = 2.9
DISCHARGE_PACK_SPREAD_THRESHOLD_MV = 10000 # only check cell spread while pack is above this
DISCHARGE_MAX_CELL_SPREAD_MV = 70

MONITOR_MAX_PACK_DROP_MV = 10

FILENAME_PREFIXES = {
    "discharging": "discharge",
    "charging": "charge",
    "monitoring": "monitor",
}


def detect_profile(csv_path):
    name = os.path.basename(csv_path)
    for prefix, profile in FILENAME_PREFIXES.items():
        if name.startswith(prefix):
            return profile
    return None


def discharged_mAh(df, t):
    """Capacity drawn from the pack: -HAT_mA_Out integrated over time."""
    hours = (t - t.iloc[0]).dt.total_seconds() / 3600
    return np.trapezoid(-df["HAT_mA_Out"], x=hours)


def charge_checks(df, t, capacity):
    max_temp = df[TEMP_COLS].max().max()
    duration_h = (t.iloc[-1] - t.iloc[0]).total_seconds() / 3600
    final_pack_v = df["vbat_mV"].iloc[-1] / 1000
    max_cell_v = df[CELL_COLS].max().max() / 1000
    return [
        Check("Max temp", max_temp <= CHARGE_MAX_TEMP_C,
              f"{max_temp:.1f} °C", f"<= {CHARGE_MAX_TEMP_C} °C"),
        Check("Charge duration", duration_h <= CHARGE_MAX_DURATION_H,
              f"{duration_h:.2f} h", f"<= {CHARGE_MAX_DURATION_H} h"),
        Check("Final pack voltage",
              abs(final_pack_v - CHARGE_FINAL_PACK_V) <= CHARGE_FINAL_PACK_TOLERANCE_V,
              f"{final_pack_v:.3f} V",
              f"{CHARGE_FINAL_PACK_V} ± {CHARGE_FINAL_PACK_TOLERANCE_V} V"),
        Check("Max cell voltage", max_cell_v <= CHARGE_MAX_CELL_V,
              f"{max_cell_v:.3f} V", f"<= {CHARGE_MAX_CELL_V} V"),
        Check("Charged capacity", capacity >= CHARGE_MIN_CAPACITY_MAH,
              f"{capacity:.0f} mAh", f">= {CHARGE_MIN_CAPACITY_MAH} mAh"),
    ]


def discharge_checks(df, t, capacity):
    max_temp = df[TEMP_COLS].max().max()
    min_cell_v = df[CELL_COLS].min().min() / 1000

    above_threshold = df["vbat_mV"] > DISCHARGE_PACK_SPREAD_THRESHOLD_MV
    pack_v_label = DISCHARGE_PACK_SPREAD_THRESHOLD_MV / 1000
    if above_threshold.any():
        spread_mV = (df.loc[above_threshold, CELL_COLS].max(axis=1)
                     - df.loc[above_threshold, CELL_COLS].min(axis=1))
        max_spread = spread_mV.max()
        spread_check = Check(f"Cell spread (pack > {pack_v_label} V)",
                             max_spread <= DISCHARGE_MAX_CELL_SPREAD_MV,
                             f"{max_spread:.0f} mV",
                             f"<= {DISCHARGE_MAX_CELL_SPREAD_MV} mV")
    else:
        spread_check = Check(f"Cell spread (pack > {pack_v_label} V)", True,
                             f"n/a (pack never > {pack_v_label} V)",
                             f"<= {DISCHARGE_MAX_CELL_SPREAD_MV} mV")

    return [
        Check("Max temp", max_temp <= DISCHARGE_MAX_TEMP_C,
              f"{max_temp:.1f} °C", f"<= {DISCHARGE_MAX_TEMP_C} °C"),
        Check("Discharged capacity", capacity >= DISCHARGE_MIN_CAPACITY_MAH,
              f"{capacity:.0f} mAh", f">= {DISCHARGE_MIN_CAPACITY_MAH} mAh"),
        Check("Min cell voltage", min_cell_v >= DISCHARGE_MIN_CELL_V,
              f"{min_cell_v:.3f} V", f">= {DISCHARGE_MIN_CELL_V} V"),
        spread_check,
    ]


def monitor_checks(delta_mV):
    """delta_mV: 6h+ pack voltage change (end - start); None if no 6 h+ data."""
    limit = f"drop <= {MONITOR_MAX_PACK_DROP_MV} mV"
    if delta_mV is None:
        return [Check("Pack drift (6 h+)", False, "no data after 6 h", limit)]
    return [Check("Pack drift (6 h+)", delta_mV >= -MONITOR_MAX_PACK_DROP_MV,
                  f"{delta_mV:+.1f} mV", limit)]


def add_checks_axis(fig, gs, row):
    """A row reserved for render_checks(), kept separate from any data panel."""
    ax = fig.add_subplot(gs[row])
    ax.set_xticks([])
    ax.set_yticks([])
    for spine in ax.spines.values():
        spine.set_visible(False)
    return ax


def render_checks(ax, checks):
    """Fill a dedicated checks axis (see add_checks_axis()) with a pass/fail
    summary, and print the same to stdout.

    Returns whether all checks passed.
    """
    overall_pass = all(c.passed for c in checks)
    lines = [f"{'PASS' if c.passed else 'FAIL'}  {c.name}: {c.measured} (limit {c.limit})"
             for c in checks]
    text = "\n".join(lines)
    ax.set_facecolor("honeydew" if overall_pass else "mistyrose")
    ax.text(0.01, 0.5, text, transform=ax.transAxes, fontsize=8,
            family="monospace", va="center", ha="left")

    print(f"{'PASS' if overall_pass else 'FAIL'}:")
    for line in lines:
        print(f"  {line}")

    return overall_pass


def shade_failures(ax, t, mask, color="red", alpha=0.15):
    """Shade the x-regions of ax where a failure mask (aligned with t) is True."""
    mask = np.asarray(mask, dtype=bool)
    if not mask.any():
        return
    idx = np.flatnonzero(mask)
    breaks = np.flatnonzero(np.diff(idx) != 1)
    starts = np.concatenate(([0], breaks + 1))
    ends = np.concatenate((breaks, [len(idx) - 1]))
    for s, e in zip(starts, ends):
        ax.axvspan(t.iloc[idx[s]], t.iloc[idx[e]], color=color, alpha=alpha, zorder=0)


def shade_panel(ax, color="#ffe0e0"):
    """Tint a whole panel's background, for failures with no specific x-location."""
    ax.set_facecolor(color)


def temps_panel(ax, df, t):
    """Internal temperatures (°C) with 0/60 °C limit lines."""
    for col, label in zip(TEMP_COLS, ["AHT", "BQ76920", "BQ25798"]):
        ax.plot(t, df[col], label=label)
    ax.set_ylim(-5, 65)
    ax.axhline(0, color="red", linewidth=1)
    ax.axhline(60, color="red", linewidth=1)
    ax.set_ylabel("Temp (°C)")
    ax.legend(loc="upper right", fontsize=8)
    ax.grid(True, alpha=0.3)


def pack_panel(ax, df, t, autorange=False):
    """Pack voltage (mV -> V) with 9 V limit line."""
    ax.plot(t, df["vbat_mV"] / 1000, label="Vbat")
    if not autorange:
        ax.set_ylim(8, 13)
        ax.axhline(9, color="red", linewidth=1)
    ax.set_ylabel("Pack (V)")
    ax.legend(loc="upper right", fontsize=8)
    ax.grid(True, alpha=0.3)


def cells_panel(ax, df, t, autorange=False):
    """Cell voltages (mV -> V) with 3 V limit line."""
    for cell in ("cell1_mV", "cell2_mV", "cell3_mV"):
        ax.plot(t, df[cell] / 1000, label=cell.split("_")[0])
    if not autorange:
        ax.set_ylim(2.8, 4.2)
        ax.axhline(3, color="red", linewidth=1)
    ax.set_ylabel("Cell (V)")
    ax.legend(loc="upper right", fontsize=8)
    ax.grid(True, alpha=0.3)


def plot_discharge(df, t, title):
    """Discharge run: constant-current discharge through the HAT."""
    capacity = discharged_mAh(df, t)
    print(f"Discharged capacity: {capacity:.0f} mAh")

    fig = plt.figure(figsize=(12, 13.8))
    gs = fig.add_gridspec(6, 1, height_ratios=[0.45, 1, 1, 1, 1, 1])
    ax_checks = add_checks_axis(fig, gs, 0)
    ax_temp = fig.add_subplot(gs[1])
    ax_i = fig.add_subplot(gs[2], sharex=ax_temp)
    ax_pack = fig.add_subplot(gs[3], sharex=ax_temp)
    ax_cell = fig.add_subplot(gs[4], sharex=ax_temp)
    ax_dev = fig.add_subplot(gs[5], sharex=ax_temp)

    temps_panel(ax_temp, df, t)
    shade_failures(ax_temp, t, df[TEMP_COLS].max(axis=1) > DISCHARGE_MAX_TEMP_C)

    # --- Discharge current (mA -> A, flipped so discharge is positive) ---
    ax_i.plot(t, -df["HAT_mA_Out"] / 1000, label="HAT out")
    ax_i.set_ylim(-0.2, 2.2)
    ax_i.set_ylabel("Discharge (A)")
    ax_i.legend(loc="upper right", fontsize=8)
    ax_i.grid(True, alpha=0.3)
    if capacity < DISCHARGE_MIN_CAPACITY_MAH:
        shade_panel(ax_i)

    pack_panel(ax_pack, df, t)
    cells_panel(ax_cell, df, t)
    shade_failures(ax_cell, t, df[CELL_COLS].min(axis=1) / 1000 < DISCHARGE_MIN_CELL_V)

    # --- Cell deviation from the average cell voltage (mV) ---
    # Beyond +/-75 mV the imbalance is not acceptable.
    cells = ["cell1_mV", "cell2_mV", "cell3_mV"]
    avg = df[cells].mean(axis=1)
    dev = df[cells].sub(avg, axis=0)
    for cell in cells:
        ax_dev.plot(t, dev[cell], label=cell.split("_")[0])
    ax_dev.axhline(0, color="gray", linewidth=1)
    ax_dev.axhline(75, color="red", linewidth=1)
    ax_dev.axhline(-75, color="red", linewidth=1)
    ax_dev.set_ylim(min(-85, dev.min().min() - 5), max(85, dev.max().max() + 5))
    ax_dev.set_ylabel("Cell - avg (mV)")
    ax_dev.set_xlabel("Time")
    ax_dev.legend(loc="upper right", fontsize=8)
    ax_dev.grid(True, alpha=0.3)

    # Cell spread while pack > threshold, matching discharge_checks()'s spread check.
    above_threshold = df["vbat_mV"] > DISCHARGE_PACK_SPREAD_THRESHOLD_MV
    spread_mV = df[cells].max(axis=1) - df[cells].min(axis=1)
    shade_failures(ax_dev, t, above_threshold & (spread_mV > DISCHARGE_MAX_CELL_SPREAD_MV))

    overall_pass = render_checks(ax_checks, discharge_checks(df, t, capacity))
    status = "PASS" if overall_pass else "FAIL"
    fig.suptitle(f"{title}  —  {capacity:.0f} mAh  [{status}]",
                color="darkgreen" if overall_pass else "darkred")

    fig.autofmt_xdate()
    return fig


def plot_monitor(df, t, title):
    """Monitoring run: cells resting, watching how they settle.

    The pack voltage is split into two panels (first 6 hours / the rest) so
    each can auto-range: the initial settling would otherwise swamp the slow
    drift that follows. Those two panels have their own time axes, so only
    the temp/cell panels share an x-axis.
    """
    fig = plt.figure(figsize=(12, 11.8))
    gs = fig.add_gridspec(5, 1, height_ratios=[0.4, 1, 1, 1, 1])
    ax_checks = add_checks_axis(fig, gs, 0)
    ax_temp = fig.add_subplot(gs[1])
    ax_pack1 = fig.add_subplot(gs[2])
    ax_pack2 = fig.add_subplot(gs[3])
    ax_cell = fig.add_subplot(gs[4], sharex=ax_temp)

    temps_panel(ax_temp, df, t)

    # --- Pack voltage: first 6 hours, then the rest ---
    early = t <= t.iloc[0] + pd.Timedelta(hours=6)
    pack_panel(ax_pack1, df[early], t[early], autorange=True)
    ax_pack1.set_ylabel("Pack 0-6 h (V)")
    delta_mV = None
    if early.all():
        ax_pack2.text(0.5, 0.5, "no data after 6 h",
                      ha="center", va="center", transform=ax_pack2.transAxes)
        shade_panel(ax_pack2)
    else:
        # Raw reading faded, with a rolling average on top. The legend
        # reports the settled drift: mean of the first SMOOTH_WINDOW
        # readings -> mean of the last SMOOTH_WINDOW.
        v = df.loc[~early, "vbat_mV"] / 1000
        smooth = v.rolling(SMOOTH_WINDOW, center=True, min_periods=1).mean()
        start_v = v.iloc[:SMOOTH_WINDOW].mean()
        end_v = v.iloc[-SMOOTH_WINDOW:].mean()
        delta_mV = (end_v - start_v) * 1000
        ax_pack2.plot(t[~early], v, color="C0", alpha=0.3, label="Vbat raw")
        ax_pack2.plot(t[~early], smooth, color="C0",
                      label=f"Vbat avg: {start_v:.3f} → {end_v:.3f} V "
                            f"({delta_mV:+.0f} mV)")
        ax_pack2.legend(loc="upper right", fontsize=8)
        ax_pack2.grid(True, alpha=0.3)
        if delta_mV < -MONITOR_MAX_PACK_DROP_MV:
            shade_panel(ax_pack2)
    ax_pack2.set_ylabel("Pack 6 h+ (V)")

    cells_panel(ax_cell, df, t, autorange=True)
    ax_cell.set_xlabel("Time")

    # The pack panels have their own time ranges, so format ticks per-axis
    # instead of using fig.autofmt_xdate() (which would hide their labels).
    ax_temp.tick_params(labelbottom=False)
    for ax in (ax_pack1, ax_pack2, ax_cell):
        plt.setp(ax.get_xticklabels(), rotation=30, ha="right")

    overall_pass = render_checks(ax_checks, monitor_checks(delta_mV))
    status = "PASS" if overall_pass else "FAIL"
    fig.suptitle(f"{title}  [{status}]",
                color="darkgreen" if overall_pass else "darkred")

    return fig


# Background shading for the BQ25798 charge phases on the charge profile.
CHARGE_PHASE_COLORS = {
    "notCharging": "lightgray",
    "trickleCharge": "plum",
    "preCharge": "gold",
    "fastChargeCC": "orange",
    "taperChargeCV": "skyblue",
    "topOffTimerActivatedCharging": "khaki",
    "chargeTerminationDone": "lightgreen",
}


def plot_charge(df, t, title):
    """Charge run: CC/CV charging via the BQ25798.

    The charge current panel is shaded by chargingStatus so the CC -> CV
    handover and termination are visible at a glance.
    """
    hours = (t - t.iloc[0]).dt.total_seconds() / 3600
    capacity = np.trapezoid(df["ibat_mA"], x=hours)
    print(f"Charged capacity (net into battery): {capacity:.0f} mAh")

    fig = plt.figure(figsize=(12, 11.8))
    gs = fig.add_gridspec(5, 1, height_ratios=[0.45, 1, 1, 1, 1])
    ax_checks = add_checks_axis(fig, gs, 0)
    ax_temp = fig.add_subplot(gs[1])
    ax_i = fig.add_subplot(gs[2], sharex=ax_temp)
    ax_v = fig.add_subplot(gs[3], sharex=ax_temp)
    ax_cell = fig.add_subplot(gs[4], sharex=ax_temp)

    temps_panel(ax_temp, df, t)
    shade_failures(ax_temp, t, df[TEMP_COLS].max(axis=1) > CHARGE_MAX_TEMP_C)

    # --- Charge current (mA -> A), with charge phases as background ---
    status = df["chargingStatus"]
    blocks = (status != status.shift()).cumsum()
    seen = {}
    for _, block in df.groupby(blocks):
        phase = block["chargingStatus"].iloc[0]
        color = CHARGE_PHASE_COLORS.get(phase, "mistyrose")
        span = ax_i.axvspan(t[block.index[0]], t[block.index[-1]],
                            color=color, alpha=0.3)
        seen.setdefault(phase, span)
    ax_i.plot(t, df["ibat_mA"] / 1000, label="Ibat")
    ax_i.plot(t, df["HAT_mA_In"] / 1000, label="HAT in")
    ax_i.set_ylim(-0.1, 2.5)
    ax_i.set_ylabel("Charge (A)")
    handles, labels = ax_i.get_legend_handles_labels()
    handles += list(seen.values())
    labels += list(seen.keys())
    ax_i.legend(handles, labels, loc="center right", fontsize=8)
    ax_i.grid(True, alpha=0.3)
    if capacity < CHARGE_MIN_CAPACITY_MAH:
        shade_panel(ax_i)

    # --- Pack and input voltage (mV -> V), 12.6 V = 3s max charge voltage ---
    ax_v.plot(t, df["vbat_mV"] / 1000, label="Vbat")
    ax_v.plot(t, df["vbus_mV"] / 1000, label="Vbus")
    ax_v.set_ylim(8, 13)
    ax_v.axhline(12.6, color="red", linewidth=1)
    ax_v.set_ylabel("Pack (V)")
    ax_v.legend(loc="lower right", fontsize=8)
    ax_v.grid(True, alpha=0.3)
    final_pack_v = df["vbat_mV"].iloc[-1] / 1000
    if abs(final_pack_v - CHARGE_FINAL_PACK_V) > CHARGE_FINAL_PACK_TOLERANCE_V:
        tail_start = int(len(t) * 0.95)
        ax_v.axvspan(t.iloc[tail_start], t.iloc[-1], color="red", alpha=0.15, zorder=0)

    # --- Cell voltages (mV -> V), 4.2 V = max cell voltage ---
    for cell in ("cell1_mV", "cell2_mV", "cell3_mV"):
        ax_cell.plot(t, df[cell] / 1000, label=cell.split("_")[0])
    ax_cell.set_ylim(2.8, 4.3)
    ax_cell.axhline(4.2, color="red", linewidth=1)
    ax_cell.set_ylabel("Cell (V)")
    ax_cell.set_xlabel("Time")
    ax_cell.legend(loc="lower right", fontsize=8)
    ax_cell.grid(True, alpha=0.3)
    shade_failures(ax_cell, t, df[CELL_COLS].max(axis=1) / 1000 > CHARGE_MAX_CELL_V)

    duration_h = (t.iloc[-1] - t.iloc[0]).total_seconds() / 3600
    if duration_h > CHARGE_MAX_DURATION_H:
        cutoff = t.iloc[0] + pd.Timedelta(hours=CHARGE_MAX_DURATION_H)
        for ax in (ax_temp, ax_i, ax_v, ax_cell):
            ax.axvspan(cutoff, t.iloc[-1], color="red", alpha=0.12, zorder=0)

    overall_pass = render_checks(ax_checks, charge_checks(df, t, capacity))
    check_status = "PASS" if overall_pass else "FAIL"
    fig.suptitle(f"{title}  —  {capacity:.0f} mAh  [{check_status}]",
                color="darkgreen" if overall_pass else "darkred")

    fig.autofmt_xdate()
    return fig


PLOTTERS = {
    "discharge": plot_discharge,
    "charge": plot_charge,
    "monitor": plot_monitor,
}

# Files to pull out of a run zip: (substring to match in the CSV's basename,
# profile to plot it with, name used for the output PNG).
ZIP_TARGETS = (
    ("full_charge", "charge"),
    ("full_discharge", "discharge"),
    ("monitoring", "monitor"),
)


def load_df(fileobj_or_path):
    df = pd.read_csv(fileobj_or_path)
    if df.empty:
        return None
    df["timestamp"] = pd.to_datetime(df["timestamp"])
    return df


def plot_and_save(df, profile, title, save_path):
    t = df["timestamp"]
    fig = PLOTTERS[profile](df, t, title)
    fig.tight_layout()
    fig.savefig(save_path, dpi=150)
    plt.close(fig)
    print(f"Saved {save_path}")


def zip_run_name(zf, zip_path):
    """Name for the run, e.g. 'Battery_118___Time_2026-07-30_16-09-29'.

    Taken from the zip's single top-level folder if it has one, otherwise
    falls back to the zip's own filename.
    """
    top_levels = {n.split("/")[0] for n in zf.namelist() if "/" in n}
    if len(top_levels) == 1:
        return top_levels.pop()
    return os.path.splitext(os.path.basename(zip_path))[0]


def process_zip(zip_path, outdir, profile_override):
    target_dir = outdir or os.path.dirname(zip_path) or "."

    with zipfile.ZipFile(zip_path) as zf:
        save_dir = os.path.join(target_dir, zip_run_name(zf, zip_path))
        os.makedirs(save_dir, exist_ok=True)

        names = [n for n in zf.namelist() if n.lower().endswith(".csv")]

        targets = ZIP_TARGETS
        if profile_override:
            targets = [(needle, prof) for needle, prof in ZIP_TARGETS
                      if prof == profile_override]

        found_any = False
        for needle, profile in targets:
            matches = [n for n in names if needle in os.path.basename(n)]
            if not matches:
                print(f"No {needle} CSV found in {zip_path}, skipping")
                continue
            name = matches[0]
            with zf.open(name) as f:
                df = load_df(f)
            if df is None:
                print(f"No rows in {name}, skipping")
                continue
            found_any = True
            title = f"{profile.capitalize()}: {os.path.basename(name)}"
            save_path = os.path.join(save_dir, f"{needle}.png")
            plot_and_save(df, profile, title, save_path)

    if not found_any:
        sys.exit(f"No matching CSVs found in {zip_path}")


def process_csv(csv_path, save, profile_override):
    profile = profile_override or detect_profile(csv_path)
    if profile is None:
        sys.exit(f"Cannot detect profile from filename {csv_path!r}; "
                 f"use --profile {{{','.join(PROFILES)}}}")

    df = load_df(csv_path)
    if df is None:
        sys.exit(f"No rows in {csv_path}")

    title = f"{profile.capitalize()}: {csv_path}"
    t = df["timestamp"]
    fig = PLOTTERS[profile](df, t, title)
    fig.tight_layout()

    if save:
        fig.savefig(save, dpi=150)
        print(f"Saved {save}")
    else:
        plt.show()


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("input", help="CSV file, or a run zip, to plot")
    ap.add_argument("--save", metavar="FILE",
                    help="save figure to FILE instead of showing (single CSV only)")
    ap.add_argument("--outdir", metavar="DIR",
                    help="target directory when input is a zip; a folder "
                         "named after the run (battery + time) is created "
                         "inside it and the graphs saved there (default "
                         "target directory: next to the zip)")
    ap.add_argument("--profile", choices=PROFILES,
                    help="run profile (default: detect from filename; for a "
                         "zip, restricts to just that profile's graph)")
    args = ap.parse_args()

    if args.input.lower().endswith(".zip"):
        process_zip(args.input, args.outdir, args.profile)
    else:
        process_csv(args.input, args.save, args.profile)


if __name__ == "__main__":
    main()
