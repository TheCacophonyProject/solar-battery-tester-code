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

import matplotlib.pyplot as plt
import numpy as np
import pandas as pd

PROFILES = ("discharge", "charge", "monitor")

# Number of readings averaged for the smoothed trace (and the start/end
# voltages in its legend) on the monitor profile's 6 h+ pack panel.
SMOOTH_WINDOW = 600 # 10 minutes as reading every 10 seconds

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


def temps_panel(ax, df, t):
    """Internal temperatures (°C) with 0/60 °C limit lines."""
    for col, label in [("tempAHT_C", "AHT"), ("tempBQ76920_C", "BQ76920"),
                       ("tempBQ25798_C", "BQ25798")]:
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

    fig, (ax_temp, ax_i, ax_pack, ax_cell, ax_dev) = plt.subplots(
        5, 1, sharex=True, figsize=(12, 13))
    fig.suptitle(f"{title}  —  {capacity:.0f} mAh")

    temps_panel(ax_temp, df, t)

    # --- Discharge current (mA -> A, flipped so discharge is positive) ---
    ax_i.plot(t, -df["HAT_mA_Out"] / 1000, label="HAT out")
    ax_i.set_ylim(-0.2, 2.2)
    ax_i.set_ylabel("Discharge (A)")
    ax_i.legend(loc="upper right", fontsize=8)
    ax_i.grid(True, alpha=0.3)

    pack_panel(ax_pack, df, t)
    cells_panel(ax_cell, df, t)

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

    fig.autofmt_xdate()
    return fig


def plot_monitor(df, t, title):
    """Monitoring run: cells resting, watching how they settle.

    The pack voltage is split into two panels (first 6 hours / the rest) so
    each can auto-range: the initial settling would otherwise swamp the slow
    drift that follows. Those two panels have their own time axes, so only
    the temp/cell panels share an x-axis.
    """
    fig = plt.figure(figsize=(12, 11))
    gs = fig.add_gridspec(4, 1)
    ax_temp = fig.add_subplot(gs[0])
    ax_pack1 = fig.add_subplot(gs[1])
    ax_pack2 = fig.add_subplot(gs[2])
    ax_cell = fig.add_subplot(gs[3], sharex=ax_temp)
    fig.suptitle(title)

    temps_panel(ax_temp, df, t)

    # --- Pack voltage: first 6 hours, then the rest ---
    early = t <= t.iloc[0] + pd.Timedelta(hours=6)
    pack_panel(ax_pack1, df[early], t[early], autorange=True)
    ax_pack1.set_ylabel("Pack 0-6 h (V)")
    if early.all():
        ax_pack2.text(0.5, 0.5, "no data after 6 h",
                      ha="center", va="center", transform=ax_pack2.transAxes)
    else:
        # Raw reading faded, with a rolling average on top. The legend
        # reports the settled drift: mean of the first SMOOTH_WINDOW
        # readings -> mean of the last SMOOTH_WINDOW.
        v = df.loc[~early, "vbat_mV"] / 1000
        smooth = v.rolling(SMOOTH_WINDOW, center=True, min_periods=1).mean()
        start_v = v.iloc[:SMOOTH_WINDOW].mean()
        end_v = v.iloc[-SMOOTH_WINDOW:].mean()
        ax_pack2.plot(t[~early], v, color="C0", alpha=0.3, label="Vbat raw")
        ax_pack2.plot(t[~early], smooth, color="C0",
                      label=f"Vbat avg: {start_v:.3f} → {end_v:.3f} V "
                            f"({(end_v - start_v) * 1000:+.0f} mV)")
        ax_pack2.legend(loc="upper right", fontsize=8)
        ax_pack2.grid(True, alpha=0.3)
    ax_pack2.set_ylabel("Pack 6 h+ (V)")

    cells_panel(ax_cell, df, t, autorange=True)
    ax_cell.set_xlabel("Time")

    # The pack panels have their own time ranges, so format ticks per-axis
    # instead of using fig.autofmt_xdate() (which would hide their labels).
    ax_temp.tick_params(labelbottom=False)
    for ax in (ax_pack1, ax_pack2, ax_cell):
        plt.setp(ax.get_xticklabels(), rotation=30, ha="right")

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

    fig, (ax_temp, ax_i, ax_v, ax_cell) = plt.subplots(
        4, 1, sharex=True, figsize=(12, 11))
    fig.suptitle(f"{title}  —  {capacity:.0f} mAh")

    temps_panel(ax_temp, df, t)

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

    # --- Pack and input voltage (mV -> V), 12.6 V = 3s max charge voltage ---
    ax_v.plot(t, df["vbat_mV"] / 1000, label="Vbat")
    ax_v.plot(t, df["vbus_mV"] / 1000, label="Vbus")
    ax_v.set_ylim(8, 13)
    ax_v.axhline(12.6, color="red", linewidth=1)
    ax_v.set_ylabel("Pack (V)")
    ax_v.legend(loc="lower right", fontsize=8)
    ax_v.grid(True, alpha=0.3)

    # --- Cell voltages (mV -> V), 4.2 V = max cell voltage ---
    for cell in ("cell1_mV", "cell2_mV", "cell3_mV"):
        ax_cell.plot(t, df[cell] / 1000, label=cell.split("_")[0])
    ax_cell.set_ylim(2.8, 4.3)
    ax_cell.axhline(4.2, color="red", linewidth=1)
    ax_cell.set_ylabel("Cell (V)")
    ax_cell.set_xlabel("Time")
    ax_cell.legend(loc="lower right", fontsize=8)
    ax_cell.grid(True, alpha=0.3)

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
