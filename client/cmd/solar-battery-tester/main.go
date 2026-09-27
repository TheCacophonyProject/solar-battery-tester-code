// solar-battery-tester - Tests a solar battery pack using the solar-battery-tester HAT.
// Copyright (C) 2025, The Cacophony Project
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program. If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/TheCacophonyProject/go-utils/logging"
	arg "github.com/alexflint/go-arg"
	"periph.io/x/host/v3"
)

const (
	// dischargeTempLimitC   = 80.0 // °C - stop discharging above this temperature
	chargeTimeoutDuration = 12 * time.Hour
	logRate               = 10 * time.Minute
	// How long the pack has to be silent before it counts as unplugged.
	batteryRemovedAfter = 30 * time.Second
)

var log = logging.NewLogger("info")
var version = "No version provided"

// Where runs are written, and where their zips wait until the server has them.
// A var rather than a const so tests can point it at a temporary directory.
var testDataDir = "/var/lib/solar-battery-tester/data"

type Args struct {
	BatterySerial string `arg:"--battery-serial" default:"/dev/serial0" help:"Serial device for battery UART"`

	// Where finished runs get posted. Normally this comes from the config
	// file; these are for overriding it for a one-off run. With no URL from
	// either, runs are kept locally to upload later.
	ConfigPath     string `arg:"--config" default:"/etc/solar-battery-tester/config.toml" help:"Path to the TOML config file"`
	ServerURL      string `arg:"--server-url" help:"Base URL of the battery run server, overriding the config file"`
	ServerPort     int    `arg:"--server-port" help:"Port of the battery run server, overriding the config file"`
	ServerUsername string `arg:"--server-username" help:"Username for the server's basic auth, overriding the config file"`
	ServerPassword string `arg:"--server-password" help:"Password for the server's basic auth, overriding the config file"`

	// Unit Tests
	CheckServer *subcommand `arg:"subcommand:check-server" help:"Check the server address and credentials from the config, and exit"`
	TestSerial  *subcommand `arg:"subcommand:test-serial" help:"Test the serial port connection and exit"`
	TestADC     *subcommand `arg:"subcommand:test-adc" help:"Read and print the values from the ADC"`
	TestOCD     *subcommand `arg:"subcommand:test-ocd" help:"Test Over Current Detection (OCD)"`
	TestSCD     *subcommand `arg:"subcommand:test-scd" help:"Test Short Circuit Detection (SCD)"`

	// Sequences
	RunChargeSeq        *subcommandDuration `arg:"subcommand:run-charge-seq" help:"Run the charge sequence and exit"`
	RunDischargeSeq     *subcommandDuration `arg:"subcommand:run-discharge-seq" help:"Run the discharge sequence and exit"`
	RunFullDischargeSeq *subcommandDuration `arg:"subcommand:run-full-discharge-seq" help:"Run the full discharge sequence and exit"`
	RunMonitorSeq       *subcommandDuration `arg:"subcommand:run-monitor-seq" help:"Run the monitor sequence and exit"`
	RunBalanceSeq       *subcommandDuration `arg:"subcommand:run-balance-seq" help:"Run the balance sequence and exit"`
	RunStorageSeq       *subcommandStorage  `arg:"subcommand:run-storage-seq" help:"Run the storage sequence (settle at 20-30% SOC) and exit"`
	CheckStorageState   *subcommandStorage  `arg:"subcommand:check-storage-state" help:"Check the pack is ready for storage or transport, without charging or discharging it"`

	RunFullTests *subcommandDuration `arg:"subcommand:run-full-test" help:"Loop through running the full test sequence."`

	// Logging
	logging.LogArgs
}

type subcommand struct {
}

type subcommandDuration struct {
	Duration int `arg:"--duration" default:"0" help:"Option to limit the duration of a test sequence in minutes."`
}

// The storage window used when it isn't given on the command line. The defaults on the
// flags below mirror these.
const (
	defaultSOCMinVolt = 10.2
	defaultSOCMaxVolt = 10.5
)

type subcommandStorage struct {
	subcommandDuration
	SOCMinVolt float64 `arg:"--soc-min-voltage" default:"10.2" help:"Resting pack voltage at 20 percent SOC, the bottom of the storage window."`
	SOCMaxVolt float64 `arg:"--soc-max-voltage" default:"10.5" help:"Resting pack voltage at 30 percent SOC, the top of the storage window."`
}

func (Args) Version() string {
	return version
}

func main() {
	err := runMain()
	if err != nil {
		log.Fatal(err)
	}
}

func runMain() error {
	var args Args
	arg.MustParse(&args)
	log = logging.NewLogger(args.LogLevel)
	log.Printf("running version: %s", version)

	// Before the hardware: a config that can't be used should say so straight
	// away, on any machine, rather than after a HAT has been found.
	cfg, err := loadConfig(args.ConfigPath)
	if err != nil {
		return err
	}
	server, err := serverFromConfig(cfg, args)
	if err != nil {
		return err
	}
	if server.enabled() {
		log.Infof("Runs will be uploaded to %s", server.url)
	} else {
		log.Warnf("No server configured in %s, runs will be kept in %s only.",
			args.ConfigPath, testDataDir)
	}

	// check-server is that check on its own, with the answer as the exit status,
	// for confirming a config without starting a test.
	if args.CheckServer != nil {
		if !server.enabled() {
			return fmt.Errorf("no server is configured in %s", args.ConfigPath)
		}
		return checkServer(server)
	}

	if server.enabled() {
		// Not fatal: a tester whose server is down or misconfigured should still
		// test batteries and hold on to the runs. But it says so now, loudly,
		// rather than leaving it to be found at the end of a test.
		if err := checkServer(server); err != nil {
			log.Errorf("Checking the server: %v", err)
			log.Errorf("Runs will be kept in %s until this is sorted out.", testDataDir)
		}
	}

	// Initialize periph
	if _, err := host.Init(); err != nil {
		return fmt.Errorf("failed to initialize periph: %v", err)
	}

	// Initialize hardware
	hw, err := newHardware()
	if err != nil {
		return fmt.Errorf("failed to initialize hardware: %v", err)
	}
	defer hw.close()

	// Initialize battery message monitor
	battStateChan := make(chan BatteryStatus, 1)
	go func() {
		if err := runBatteryMonitor(args.BatterySerial, battStateChan); err != nil {
			log.Errorf("Battery monitor error: %v", err)
		}
	}()

	// Make data folder if it doesn't exist
	if err := os.MkdirAll(testDataDir, 0o755); err != nil {
		return fmt.Errorf("error creating data directory: %v", err)
	}

	// ==== Different Test Modes ====

	// Test reading serial from battery
	if args.TestSerial != nil {
		for {
			battState := <-battStateChan
			log.Printf("Battery State: %s", battState)
		}
	}

	// Test reading temperature from the CC load.
	if args.TestADC != nil {
		tempC, err := hw.readCCTemperature()
		if err != nil {
			return fmt.Errorf("reading temperature: %v", err)
		}
		log.Printf("Temperature: %.1f°C", tempC)

		hatC, err := hw.readHatTemperature()
		if err != nil {
			return fmt.Errorf("reading temperature: %v", err)
		}
		log.Printf("HAT Temperature: %.1f°C", hatC)

		v, err := hw.readBatteryVoltage()
		if err != nil {
			return fmt.Errorf("reading battery voltage: %v", err)
		}
		log.Printf("Battery voltage: %.3fV", v)

		return nil
	}

	// Short Circuit Test
	if args.TestSCD != nil {
		pass, err := hw.testShortCircuit(battStateChan)
		if err != nil {
			return fmt.Errorf("short circuit test errored: %v", err)
		}
		if !pass {
			return fmt.Errorf("short circuit test failed")
		}
		return nil
	}

	// Over current discharge test
	if args.TestOCD != nil {
		pass, err := hw.overCurrentDischargeTest(battStateChan)
		if err != nil {
			return fmt.Errorf("OCD test errored: %v", err)
		}
		if !pass {
			return fmt.Errorf("OCD test failed")
		}
		log.Info("OCD test passed.")
		return nil
	}

	// Run Charge Sequence
	if args.RunChargeSeq != nil {
		return hw.runChargeSeq(battStateChan, 0, "./", "charge", args.RunChargeSeq.Duration)
	}

	// Run Discharge Sequence
	if args.RunDischargeSeq != nil {
		return hw.runDischargeSeq(battStateChan, "./", "discharge", 4, args.RunDischargeSeq.Duration, false)
	}

	// Run Full Discharge Sequence
	if args.RunFullDischargeSeq != nil {
		return hw.runDischargeSeq(battStateChan, "./", "discharge", 4, args.RunFullDischargeSeq.Duration, true)
	}

	// Run Monitor Sequence
	if args.RunMonitorSeq != nil {
		return hw.runMonitorTest(battStateChan, "./", args.RunMonitorSeq.Duration)
	}

	// Run Balance Sequence
	if args.RunBalanceSeq != nil {
		return hw.waitForCellsToBalance(battStateChan, "./", args.RunBalanceSeq.Duration)
	}

	// Run Storage Sequence
	if args.RunStorageSeq != nil {
		return hw.runStorageSeq(battStateChan, "./", args.RunStorageSeq.SOCMinVolt, args.RunStorageSeq.SOCMaxVolt, args.RunStorageSeq.Duration)
	}

	// Check the pack is ready for storage
	if args.CheckStorageState != nil {
		return hw.checkStorageState(battStateChan, args.CheckStorageState.SOCMinVolt, args.CheckStorageState.SOCMaxVolt)
	}

	if args.RunFullTests != nil {
		for {
			// Anything a previous run couldn't hand over, because the network or
			// the server was down, goes now.
			uploadPending(server)

			// Run full test
			err := runFullTest(hw, battStateChan, args, server)
			if err != nil {
				log.Errorf("Full test failed/errored: %v", err)
				hw.flashLED(200, 0, 0)
			} else {
				log.Info("Full test passed.")
				hw.flashLED(0, 200, 0)
			}

			// Wait for the pack to be unplugged before starting on the next one,
			// so a finished test isn't immediately followed by another run on the
			// same battery.
			log.Info("Waiting for the battery to be unplugged.")
			waitForBatteryRemoval(battStateChan)
			log.Info("Battery unplugged.")
		}
	}

	return nil
}

func runFullTest(hw *hardware, battStateChan chan BatteryStatus, args Args, server serverConfig) error {
	log.Info("=== Full Test Sequence Setup ===\n")
	hw.solidLED(true, false, false)

	log.Info("=== Waiting for battery to be plugged in ===")
	batteryState := <-battStateChan
	log.Infof("Battery detected: %s\n", batteryState)
	hw.flashLED(0, 0, 1000)

	log.Info("=== Running Full Test Sequence ===\n")

	batteryID := int(batteryState.BatteryID)
	// The run's identity: the server takes all three of these out of
	// results.json, and the folder name is built from the same values so the
	// two can't disagree.
	results := &testResults{
		BatteryID:  batteryID,
		TesterName: testerName(),
		Timestamp:  time.Now(),
	}

	resultsFolderName := runFolderName(batteryID, results.TesterName, results.Timestamp)

	resultsDir := filepath.Join(testDataDir, resultsFolderName)
	if err := os.MkdirAll(resultsDir, 0o755); err != nil {
		return fmt.Errorf("error creating results directory: %v", err)
	}
	log.Infof("Saving results to: %s", resultsDir)
	time.Sleep(time.Second)

	// A test that fails is the one whose readings are most worth looking at, so
	// the results are zipped and sent whichever way the sequence below turns out.
	testErr := runTestSteps(hw, battStateChan, args, results, resultsDir)
	results.Completed = testErr == nil
	if testErr != nil {
		results.FailureReason = testErr.Error()
	}

	log.Println("=== Results ===")
	results.print()

	if err := results.save(resultsDir); err != nil {
		log.Errorf("saving results.json: %v", err)
	}

	zipPath := filepath.Join(testDataDir, resultsFolderName+".zip")
	log.Infof("Zipping results to: %s", zipPath)
	if err := zipDir(resultsDir, zipPath); err != nil {
		log.Errorf("zipping results directory: %v", err)
		return testErr
	}

	log.Infof("Uploading %s", filepath.Base(zipPath))
	verdict, err := uploadRun(server, zipPath, batteryID)
	if err != nil {
		// The zip stays in testDataDir and the next run will offer it again, so
		// a server that's down doesn't cost the run's data. The sequence itself
		// is what it is, so its own result still stands.
		log.Errorf("Uploading run: %v", err)
		return testErr
	}
	verdict.log()

	// The pack can get through the whole sequence and still be a pack that
	// failed: the capacity and temperature checks are the server's to make. So
	// its verdict decides the run, and a failing one flashes red like any other
	// failure.
	if testErr == nil {
		return verdict.verdictError()
	}

	return testErr
}

// runFolderName is the name a run is known by, on the tester and on the server,
// e.g. "Battery_126_Tester_bt-6329_Time_2026-09-08_09-27-56". The tester is in
// the name so a run can be traced back to the rig that produced it from the
// filename alone, without opening the zip.
func runFolderName(batteryID int, tester string, at time.Time) string {
	return fmt.Sprintf("Battery_%d_Tester_%s_Time_%s", batteryID, tester, at.Format("2006-01-02_15-04-05"))
}

// batteryIDFromRunName reads the battery ID back out of a run's name, for zips
// found in testDataDir that are waiting to be uploaded.
func batteryIDFromRunName(name string) (int, error) {
	m := runNamePattern.FindStringSubmatch(name)
	if m == nil {
		return 0, fmt.Errorf("%q isn't a run name of the form Battery_<id>_Tester_<name>_Time_<time>", name)
	}
	return strconv.Atoi(m[1])
}

// runNamePattern also matches the older Battery_<id>___Time_<time> names, so a
// zip written by a previous version and still waiting to be uploaded is not
// stranded by the rename.
var runNamePattern = regexp.MustCompile(`^Battery_(\d+)(?:_Tester_[A-Za-z0-9.-]+)?_+Time_`)

// waitForBatteryRemoval blocks until the pack has stopped reporting for
// batteryRemovedAfter, i.e. it has been unplugged from the tester.
func waitForBatteryRemoval(battStateChan chan BatteryStatus) {
	for {
		select {
		case <-battStateChan:
		case <-time.After(batteryRemovedAfter):
			return
		}
	}
}

// runTestSteps runs the test sequence itself, recording what it finds in results
// and its readings in resultsDir.
func runTestSteps(hw *hardware, battStateChan chan BatteryStatus, args Args, results *testResults, resultsDir string) error {
	testDuration := args.RunFullTests.Duration

	step := 1
	log.Infof("=== Step %d: Waiting for cells to be balanced ===", step)
	if err := hw.waitForCellsToBalance(battStateChan, resultsDir, testDuration); err != nil {
		return fmt.Errorf("error waiting for cells to balance: %v", err)
	}
	time.Sleep(time.Second)

	step++
	log.Infof("=== Step %d: Initial Battery Discharge ===", step)
	if err := hw.runDischargeSeq(battStateChan, resultsDir, "initial_discharge", 4, testDuration, true); err != nil {
		return fmt.Errorf("charge step failed: %v", err)
	}
	time.Sleep(time.Second)

	step++
	log.Infof("=== Step %d: Full Battery Charge ===", step)
	if err := hw.runChargeSeq(battStateChan, 0, resultsDir, "full_charge", testDuration); err != nil {
		return fmt.Errorf("charge step failed: %v", err)
	}
	time.Sleep(time.Second)

	step++
	log.Infof("=== Step %d: Checking over-current discharge protection at 3A ===", step)
	pass, err := hw.overCurrentDischargeTest(battStateChan)
	if err != nil {
		return fmt.Errorf("OCD test errored: %v", err)
	}
	results.OCDPassed = pass
	time.Sleep(time.Second)

	step++
	log.Infof("=== Step %d: Checking short circuit protection ===", step)
	pass, err = hw.testShortCircuit(battStateChan)
	if err != nil {
		return fmt.Errorf("short circuit test errored: %v", err)
	}
	results.SCDPassed = pass
	time.Sleep(time.Second)

	step++
	log.Infof("=== Step %d: Discharging battery at 2A ===", step)
	if err := hw.runDischargeSeq(battStateChan, resultsDir, "full_discharge", 4, testDuration, false); err != nil {
		return fmt.Errorf("discharge step failed: %v", err)
	}
	time.Sleep(time.Second)

	step++
	log.Infof("=== Step %d: Charging to Storage voltage (20-30%% SOC) ===", step)
	// Slightly tighter voltage range so it should pass after it has settled down.
	minVolt := defaultSOCMinVolt + 0.2*(defaultSOCMaxVolt-defaultSOCMinVolt)
	maxVolt := defaultSOCMaxVolt - 0.2*(defaultSOCMaxVolt-defaultSOCMinVolt)
	if err := hw.runStorageSeq(battStateChan, resultsDir, minVolt, maxVolt, testDuration); err != nil {
		return fmt.Errorf("storage charge step failed: %v", err)
	}
	time.Sleep(time.Second)

	step++
	log.Infof("=== Step %d: Monitoring ===", step)
	if err := hw.runMonitorTest(battStateChan, resultsDir, testDuration); err != nil {
		return fmt.Errorf("monitor step failed: %v", err)
	}

	return nil
}

// type voltageReading struct {
// 	time    time.Time
// 	voltage float64
// }

// testResults is the summary written into each run as results.json. It is what
// the server files the run by -- the battery, the tester and when the test
// started all come from here rather than from the name of the zip -- and what
// it shows for the run without unpacking the CSVs. The shape is the one
// documented in client/README.md.
type testResults struct {
	Completed  bool      `json:"completed"`  // the test sequence ran to the end
	OCDPassed  bool      `json:"ocdPassed"`  // over current protection passed
	SCDPassed  bool      `json:"scdPassed"`  // short circuit protection passed
	BatteryID  int       `json:"batteryID"`  // ID of the battery, from its EEPROM
	TesterName string    `json:"testerName"` // hostname of the RPi that ran the test
	Timestamp  time.Time `json:"timestamp"`  // when the test started, RFC 3339

	// Why the sequence stopped, when it didn't finish. Not in the README's
	// list because it's only there on a run that failed, but it is the first
	// thing anyone looking at such a run wants to know.
	FailureReason string `json:"failureReason,omitempty"`
}

func (r *testResults) print() {
	log.Println("=== Test Results ===")
	log.Printf("Battery:                %d", r.BatteryID)
	log.Printf("Tester:                 %s", r.TesterName)
	log.Printf("Started:                %s", r.Timestamp.Format(time.RFC3339))
	log.Printf("OCD protection (3A):    %s", passFailStr(r.OCDPassed))
	log.Printf("Short circuit protect:  %s", passFailStr(r.SCDPassed))
	log.Printf("Full test:              %s", passFailStr(r.Completed))
	if r.FailureReason != "" {
		log.Printf("Failed because:         %s", r.FailureReason)
	}
}

// save writes the results as JSON into dir.
func (r *testResults) save(dir string) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling results: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "results.json"), data, 0o644); err != nil {
		return fmt.Errorf("writing results.json: %v", err)
	}
	return nil
}

// zipDir writes the contents of srcDir into a new zip file at destZip, with
// srcDir's own name as the top-level folder inside the archive.
func zipDir(srcDir, destZip string) error {
	zipFile, err := os.Create(destZip)
	if err != nil {
		return fmt.Errorf("creating zip file: %v", err)
	}
	defer zipFile.Close()

	zw := zip.NewWriter(zipFile)
	defer zw.Close()

	baseDir := filepath.Dir(srcDir)
	return filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		relPath, err := filepath.Rel(baseDir, path)
		if err != nil {
			return err
		}
		w, err := zw.Create(relPath)
		if err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(w, f)
		return err
	})
}

func passFailStr(pass bool) string {
	if pass {
		return "PASS"
	}
	return "FAIL"
}
