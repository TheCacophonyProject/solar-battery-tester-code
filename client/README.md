# Solar Battery Tester Client

This is the code that will run on a Raspberry Pi with a battery tester HAT.

## The LED

| | |
| --- | --- |
| solid red | setting up, waiting for a battery |
| flashing blue | battery found, test running |
| flashing green | the test finished and the server's checks passed |
| flashing red | the test failed, or the server's checks on it failed |

## Test results

When the test is done it will zip the results and upload it to the server to see if the test passed.

The results will have a JSON file `results.json` and multiple `.csv` files.

`results.json`

```json
{
    completed: bool,       // If the test sequence finished.
    ocdPassed: bool,       // If the over current protection passed.
    scdPassed: bool,       // If the short circuit protection passed.
    batteryID: int,        // ID of the battery
    testerName: string,    // Hostname of the RPi that ran the test.
    timestamp: string,     // Timestamp of when the test started, RFC 3339.
    failureReason: string, // Why the sequence stopped. Only on a run that failed.
}
```

This is what the server files the run by: the battery, the tester and the start
time all come out of here, so the zip itself can be called anything. The name
the tester gives it, `Battery_<id>_Tester_<name>_Time_<time>.zip`, is only a
convenience for reading a directory listing.

CSV files:

- `initial_discharge.csv`
- `charge.csv`        // Used to make a graph
- `discharge.csv`     // Used to make a graph
- `storage_charge.csv`
- `monitoring.csv`    // Used to make a graph

## Configuration

Which server the runs go to, and the credentials for it, come from
`/etc/solar-battery-tester/config.toml`. `_release/config.toml.example` is the
annotated version of it:

```toml
[server]
url = "https://battery.example.com"
username = "tester"
password = "hunter2"
```

With no `url` set — or no config file at all — the tester still runs, keeping
its runs in `/var/lib/solar-battery-tester/data` and uploading them at the
start of a later test once one is set. A config file that *is* there but can't
be parsed, or that has a key the tester doesn't know, stops it with an error
rather than leaving it quietly not uploading.

### Running it by hand

Nothing to load first — the tester reads the config file itself:

```bash
sudo solar-battery-tester run-full-test
sudo solar-battery-tester --config ./my-config.toml run-full-test
```
