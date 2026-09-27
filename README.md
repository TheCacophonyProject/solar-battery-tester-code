# solar-battery-tester-code

## Client

The client is a Raspberry Pi that is running the test on a battery. Once the test is done it will upload the results to the server where it will get a response back if the battery passed or failed.

Sometimes the battery will fail early if it didn't pass the Short Circuit Protection or Over Current Detection.

It upload the battery logs along with the clients ID and test date for traceability.

## Server

The server is where all the batteries test data gets uploaded to and is then checked if it passed or not.

The user can then log onto the server to recall the test data for each battery. A searchable dropdown of the battery ID will then show a page with all the tests for that battery and clicking on a test will bring up the resulting test image.

## Repository layout

``` text
client/   the tester itself, running on a Raspberry Pi (Go)
  cmd/solar-battery-tester/   the test sequence, hardware drivers and the upload
  _release/                   systemd unit and postinstall for the .deb
  image_setup/                how a tester Pi's SD card image is made
server/   where the runs get uploaded to and looked at (Python)
  webapp.py                   uploads, the run archive and the JSON API
  templates/index.html        the browse page
  static/                     its stylesheet and JavaScript
  plot_results.py             the plots and the pass/fail checks
  deploy/                     systemd unit and nginx config
  DEPLOY.md                   how to set a server up
```

`go.mod` and `.goreleaser.yml` stay at the top level: the shared Cacophony release
workflow builds from the repository root.

## Battery Testing Phases

1) Cell Balancing: Wait until the cells are balanced.
2) Pack Full Discharge: Do a full discharge of the battery pack (decreasing discharge rate at the end to discharge the pack fully)
3) Pack Charge: Do a full charge at the maximum charge rate.
4) Over Current Protection check.
5) Short Circuit Protection check.
6) Pack 2A discharge. Discharge the pack at 2A until under voltage protection disconnects the pack.
7) Charge pack to storage voltage: Charge the pack to 20-30% SOC for safe storage/transport.
8) Pack Monitoring: Monitor the pack for 12 hours to check that the pack is stable.
