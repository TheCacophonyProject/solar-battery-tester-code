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
	"periph.io/x/conn/v3/gpio"
	"periph.io/x/conn/v3/gpio/gpioreg"
	"periph.io/x/host/v3"
)

const (
	chargeTargetVoltage   = 12.6 // V - full charge target
	chargeCutoffCurrent   = 0.1  // A - stop charging below this current
	dischargeMinVoltage   = 9.0  // V - stop discharging below this voltage
	dischargeTempLimitC   = 80.0 // °C - stop discharging above this temperature
	ocdTestTimeout        = 5 * time.Second
	shortCircuitTimeout   = 2 * time.Second
	chargeMonitorInterval = 5 * time.Minute
	chargeMonitorDuration = 10 * time.Minute
	logInterval           = 10 * time.Second
	chargeTimeoutDuration = 12 * time.Hour
	dataDir               = "/var/lib/solar-battery-tester/data"
	restVoltage           = 11.3
	logRate               = 30 * time.Minute
)

var log = logging.NewLogger("info")
var version = "No version provided"

type Args struct {
	BatterySerial string `arg:"--battery-serial" default:"/dev/serial0" help:"Serial device for battery UART"`

	// Tests
	TestSerial     bool `arg:"--test-serial" help:"Test the serial port connection and exit"`
	TestTemp       bool `arg:"--test-temp" help:"Read and print the temperature from the ADC then exit"`
	TestBatteryV   bool `arg:"--test-battery-voltage" help:"Read and print the battery voltage from the ADC then exit"`
	TestCharge     bool `arg:"--test-charge" help:"Test that we can charge the battery"`
	TestDischarge  bool `arg:"--test-discharge" help:"Test that we can discharge the battery"`
	TestOCD        bool `arg:"--test-ocd" help:"Test Over Current Detection (OCD)"`
	TestSCD        bool `arg:"--test-scd" help:"Test Short Circuit Detection (SCD)"`
	RunMonitorTest bool `arg:"--run-monitor-test" help:"Run the monitor test and exit"`

	// Sequences
	RunChargeSeq    bool `arg:"--run-charge-seq" help:"Run the charge sequence and exit"`
	RunDischargeSeq bool `arg:"--run-discharge-seq" help:"Run the discharge sequence and exit"`

	// Logging
	logging.LogArgs
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

	// GPIO7 is wired up but is no longer used so we put in a high impedance input.
	gpio7 := gpioreg.ByName(pinBatterySenseDigital)
	if gpio7 == nil {
		return fmt.Errorf("GPIO pin %s not found", pinBatterySenseDigital)
	}
	if err := gpio7.In(gpio.Float, gpio.NoEdge); err != nil {
		return fmt.Errorf("set %s as high-impedance input: %v", pinBatterySenseDigital, err)
	}

	// Initialize hardware
	hw, err := newHardware()
	if err != nil {
		return fmt.Errorf("failed to initialize hardware: %v", err)
	}
	defer hw.close()

	// Initialize battery message monitor
	battStateChan := make(chan BatteryStatus, 100)
	go func() {
		if err := runBatteryMonitor(args.BatterySerial, battStateChan); err != nil {
			log.Errorf("Battery monitor error: %v", err)
		}
	}()

	// Make data folder if it doesn't exist
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("error creating data directory: %v", err)
	}

	// ==== Different Test Modes ====

	// Test reading serial from battery
	if args.TestSerial {
		for {
			battState := <-battStateChan
			log.Printf("Battery State: %s", battState)
		}
	}

	// Test reading temperature from the CC load.
	if args.TestTemp {
		tempC, err := hw.readTemperature()
		if err != nil {
			return fmt.Errorf("reading temperature: %v", err)
		}
		log.Printf("Temperature: %.1f°C", tempC)
		return nil
	}

	// Test Reading Battery Voltage
	if args.TestBatteryV {
		v, err := hw.readBatteryVoltage()
		if err != nil {
			return fmt.Errorf("reading battery voltage: %v", err)
		}
		log.Printf("Battery voltage: %.3fV", v)
		return nil
	}

	// Test Charging the battery
	if args.TestCharge {
		pass, err := hw.testCharge(battStateChan)
		if err != nil {
			return fmt.Errorf("charge test errored: %v", err)
		}
		if !pass {
			return fmt.Errorf("charge test failed")
		}
		return nil
	}

	// Test Discharging the battery
	if args.TestDischarge {
		pass, err := hw.testDischarge(battStateChan)
		if err != nil {
			return fmt.Errorf("discharge test errored: %v", err)
		}
		if !pass {
			return fmt.Errorf("discharge test failed")
		}
		return nil
	}

	// Short Circuit Test
	if args.TestSCD {
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
	if args.TestOCD {
		pass, err := hw.overCurrentDischargeTest(battStateChan)
		if err != nil {
			return fmt.Errorf("OCD test errored: %v", err)
		}
		if !pass {
			return fmt.Errorf("OCD test failed")
		}
		return nil
	}

	// Run Charge Sequence
	if args.RunChargeSeq {
		return hw.runChargeSeq(battStateChan, 0, "./", "charge")
	}

	// Run Discharge Sequence
	if args.RunDischargeSeq {
		return hw.runDischargeSeq(battStateChan, "./", "discharge", 4)
	}

	if args.RunMonitorTest {
		return hw.runMonitorTest(battStateChan, "./")
	}

	// Run Full Test Sequence
	results := &testResults{}

	resultsDir := filepath.Join(dataDir, time.Now().Format("2006-01-02_15-04-05"))
	if err := os.MkdirAll(resultsDir, 0o755); err != nil {
		return fmt.Errorf("error creating results directory: %v", err)
	}
	log.Infof("Saving results to %s", resultsDir)
	time.Sleep(time.Second)

	step := 1
	log.Infof("=== Step %d: Initial Battery Discharge ===", step)
	if err := hw.runDischargeSeq(battStateChan, resultsDir, "initial_discharge", 2); err != nil {
		return fmt.Errorf("charge step failed: %v", err)
	}
	time.Sleep(time.Second)

	step++
	log.Infof("=== Step %d: Full Battery Charge ===", step)
	if err := hw.runChargeSeq(battStateChan, 0, resultsDir, "full_charge"); err != nil {
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
	if err := hw.runDischargeSeq(battStateChan, resultsDir, "full_discharge", 4); err != nil {
		return fmt.Errorf("discharge step failed: %v", err)
	}
	time.Sleep(time.Second)

	step++
	log.Infof("=== Step %d: Charging to rest voltage (%.1fV) ===", step, restVoltage)
	if err := hw.runChargeSeq(battStateChan, restVoltage, resultsDir, "rest_charge"); err != nil {
		return fmt.Errorf("charge step failed: %v", err)
	}
	time.Sleep(time.Second)

	step++
	log.Infof("=== Step %d: Monitoring ===", step)
	if err := hw.runMonitorTest(battStateChan, resultsDir); err != nil {
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
