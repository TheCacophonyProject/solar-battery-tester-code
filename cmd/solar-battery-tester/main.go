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
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/TheCacophonyProject/go-utils/logging"
	arg "github.com/alexflint/go-arg"
	"periph.io/x/host/v3"
)

const (
	dischargeTempLimitC   = 80.0 // °C - stop discharging above this temperature
	chargeTimeoutDuration = 12 * time.Hour
	testDataDir           = "/var/lib/solar-battery-tester/data"
	restVoltage           = 11.3
	logRate               = 20 * time.Minute
)

var log = logging.NewLogger("info")
var version = "No version provided"

type Args struct {
	BatterySerial string `arg:"--battery-serial" default:"/dev/serial0" help:"Serial device for battery UART"`

	// Unit Tests
	TestSerial *subcommand `arg:"subcommand:test-serial" help:"Test the serial port connection and exit"`
	TestADC    *subcommand `arg:"subcommand:test-adc" help:"Read and print the values from the ADC"`
	TestOCD    *subcommand `arg:"subcommand:test-ocd" help:"Test Over Current Detection (OCD)"`
	TestSCD    *subcommand `arg:"subcommand:test-scd" help:"Test Short Circuit Detection (SCD)"`

	// Sequences
	RunChargeSeq        *subcommandDuration `arg:"subcommand:run-charge-seq" help:"Run the charge sequence and exit"`
	RunDischargeSeq     *subcommandDuration `arg:"subcommand:run-discharge-seq" help:"Run the discharge sequence and exit"`
	RunFullDischargeSeq *subcommandDuration `arg:"subcommand:run-full-discharge-seq" help:"Run the full discharge sequence and exit"`
	RunMonitorSeq       *subcommandDuration `arg:"subcommand:run-monitor-seq" help:"Run the monitor sequence and exit"`
	RunBalanceSeq       *subcommandDuration `arg:"subcommand:run-balance-seq" help:"Run the balance sequence and exit"`

	RunFullTests *subcommandDuration `arg:"subcommand:run-full-test" help:"Loop through running the full test sequence."`

	// Logging
	logging.LogArgs
}

type subcommand struct {
}

type subcommandDuration struct {
	Duration int `arg:"--duration" default:"0" help:"Option to limit the duration of a test sequence in minutes."`
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

	// // Test Charging the battery
	// if args.TestCharge != nil {
	// 	pass, err := hw.testCharge(battStateChan)
	// 	if err != nil {
	// 		return fmt.Errorf("charge test errored: %v", err)
	// 	}
	// 	if !pass {
	// 		return fmt.Errorf("charge test failed")
	// 	}
	// 	return nil
	// }

	// // Test Discharging the battery
	// if args.TestDischarge != nil {
	// 	pass, err := hw.testDischarge(battStateChan)
	// 	if err != nil {
	// 		return fmt.Errorf("discharge test errored: %v", err)
	// 	}
	// 	if !pass {
	// 		return fmt.Errorf("discharge test failed")
	// 	}
	// 	return nil
	// }

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

	if args.RunFullTests != nil {
		for {
			// Run full test
			err := runFullTest(hw, battStateChan, args.RunFullTests.Duration)
			if err != nil {
				log.Errorf("Full test failed/errored: %v", err)
				hw.flashLED(200, 0, 0)
			} else {
				log.Info("Full test passed.")
				hw.flashLED(0, 200, 0)
			}

			// Wait for USB to be disconnected
			log.Info("Waiting for USB to be disconnected.")
			if err := waitForUSBRemoval(); err != nil {
				log.Errorf("waiting for USB removal: %v", err)
			}
			log.Info("USB disconnected.")
		}
	}

	return nil
}

func runFullTest(hw *hardware, battStateChan chan BatteryStatus, testDuration int) error {
	log.Info("=== Full Test Sequence Setup ===\n")
	hw.solidLED(true, false, false)

	log.Info("=== Waiting for USB device to be connected. ===")
	usbMountPath, err := waitForUSBDrive()
	if err != nil {
		return fmt.Errorf("waiting for USB device: %v", err)
	}
	log.Infof("Found USB device, mounted at %s.\n", usbMountPath)
	hw.flashLED(1000, 0, 0)

	// The drive gets pulled after every run so its data can be copied off, then
	// plugged back in for the next battery. Unmount cleanly and wait for it to
	// actually be removed before returning, regardless of how the test below
	// turns out, so it's never yanked while still mounted.
	defer func() {
		log.Info("=== Unmounting USB drive — safe to remove it now ===")
		if err := unmountUSBDrive(); err != nil {
			log.Errorf("unmounting USB drive: %v", err)
		}
	}()

	log.Info("=== Waiting for battery to be plugged in ===")
	batteryState := <-battStateChan
	log.Infof("Battery detected: %s\n", batteryState)
	hw.flashLED(0, 0, 1000)

	log.Info("=== Running Full Test Sequence ===\n")

	results := &testResults{}

	resultsFolderName := fmt.Sprintf("Battery_%d___Time_%s", batteryState.BatteryID, time.Now().Format("2006-01-02_15-04-05"))

	resultsDir := filepath.Join(usbMountPath, resultsFolderName)
	if err := os.MkdirAll(resultsDir, 0o755); err != nil {
		return fmt.Errorf("error creating results directory: %v", err)
	}
	log.Infof("Saving results to: %s", resultsDir)
	time.Sleep(time.Second)

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
	results.ocdPass = pass
	time.Sleep(time.Second)

	step++
	log.Infof("=== Step %d: Checking short circuit protection ===", step)
	pass, err = hw.testShortCircuit(battStateChan)
	if err != nil {
		return fmt.Errorf("short circuit test errored: %v", err)
	}
	results.shortCircuitPass = pass
	time.Sleep(time.Second)

	step++
	log.Infof("=== Step %d: Discharging battery at 2A ===", step)
	if err := hw.runDischargeSeq(battStateChan, resultsDir, "full_discharge", 4, testDuration, false); err != nil {
		return fmt.Errorf("discharge step failed: %v", err)
	}
	time.Sleep(time.Second)

	step++
	log.Infof("=== Step %d: Charging to rest voltage (%.1fV) ===", step, restVoltage)
	if err := hw.runChargeSeq(battStateChan, restVoltage, resultsDir, "rest_charge", testDuration); err != nil {
		return fmt.Errorf("charge step failed: %v", err)
	}
	time.Sleep(time.Second)

	step++
	log.Infof("=== Step %d: Monitoring ===", step)
	if err := hw.runMonitorTest(battStateChan, resultsDir, testDuration); err != nil {
		return fmt.Errorf("monitor step failed: %v", err)
	}
	time.Sleep(time.Second)

	log.Println("=== Results ===")
	results.print()
	return nil
}

type voltageReading struct {
	time    time.Time
	voltage float64
}

type testResults struct {
	chargeTime           time.Duration
	chargeVoltage        float64
	ocdPass              bool
	shortCircuitPass     bool
	dischargeTime        time.Duration
	dischargeEndVoltage  float64
	dischargeTempTripped bool
	monitorReadings      []voltageReading
}

func (r *testResults) print() {
	log.Println("=== Test Results ===")
	log.Printf("Charge time:            %s", r.chargeTime.Round(time.Second))
	log.Printf("Charge end voltage:     %.3fV", r.chargeVoltage)
	log.Printf("OCD protection (3A):    %s", passFailStr(r.ocdPass))
	log.Printf("Short circuit protect:  %s", passFailStr(r.shortCircuitPass))
	log.Printf("Discharge time:         %s", r.dischargeTime.Round(time.Second))
	log.Printf("Discharge end voltage:  %.3fV", r.dischargeEndVoltage)
	if r.dischargeTempTripped {
		log.Printf("Discharge temp limit:   TRIPPED (stopped at %.0f°C)", dischargeTempLimitC)
	}
	if len(r.monitorReadings) > 0 {
		first := r.monitorReadings[0].voltage
		last := r.monitorReadings[len(r.monitorReadings)-1].voltage
		log.Printf("24h voltage drift:      %.3fV (%.3fV -> %.3fV)", last-first, first, last)
	}
}

func passFailStr(pass bool) string {
	if pass {
		return "PASS"
	}
	return "FAIL"
}
