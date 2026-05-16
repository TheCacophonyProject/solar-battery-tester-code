package main

import (
	"encoding/csv"
	"fmt"
	"math"
	"math/rand"
	"os"
	"slices"
	"strconv"
	"time"

	"periph.io/x/conn/v3/gpio"
	"periph.io/x/conn/v3/gpio/gpioreg"
	"periph.io/x/conn/v3/i2c"
	"periph.io/x/conn/v3/i2c/i2creg"
)

// GPIO pin assignments from schematic (solar-battery-tester.kicad_sch).
const (
	pinChargeEn            = "GPIO17"
	pinEnShort             = "GPIO5"
	pinEnFan               = "GPIO4"
	pinBatterySenseDigital = "GPIO7"
	pinAdcReady            = "GPIO27"
	pinLedR                = "GPIO25"
	pinLedG                = "GPIO24"
	pinLedB                = "GPIO23"
	pinCCLoad1             = "GPIO21"
	pinCCLoad2             = "GPIO20"
	pinCCLoad3             = "GPIO26"
	pinCCLoad4             = "GPIO16"
	pinCCLoad5             = "GPIO19"
	pinCCLoad6             = "GPIO6"
)

// Shunt resistance on the solar-battery-tester PCB (R412 for charge, R420 for discharge).
const pcbShuntOhms = 0.060

// I2C device addresses.
const (
	i2cAddrADS1115         = 0x48 // ADDR pin tied to GND
	i2cAddrINA219Charge    = 0x40 // U404: A0=GND, A1=GND
	i2cAddrINA219Discharge = 0x41 // U405: A0=VDD, A1=GND
)

// Battery voltage divider: R417=470kΩ (top), R418=100kΩ (bottom).
// const battVoltDividerRatio = (470.0 + 100.0) / 100.0
const battVoltDividerRatio = 5.7

type hardware struct {
	bus                 i2c.BusCloser
	chargeEn            gpio.PinOut
	enShort             gpio.PinOut
	enFan               gpio.PinOut
	batterySenseDigital gpio.PinIn
	adcReady            gpio.PinIn
	ledR                gpio.PinOut
	ledG                gpio.PinOut
	ledB                gpio.PinOut
	ccLoads             [6]gpio.PinOut

	chargeMonitor    *INA219
	dischargeMonitor *INA219
	adc              *ADS1115
}

func newHardware() (*hardware, error) {
	bus, err := i2creg.Open("")
	if err != nil {
		return nil, fmt.Errorf("open I2C bus: %v", err)
	}

	hw := &hardware{bus: bus}

	outputPinDefs := []struct {
		name string
		pin  *gpio.PinOut
	}{
		{pinChargeEn, &hw.chargeEn},
		{pinEnShort, &hw.enShort},
		{pinEnFan, &hw.enFan},
		{pinLedR, &hw.ledR},
		{pinLedG, &hw.ledG},
		{pinLedB, &hw.ledB},
		{pinCCLoad1, &hw.ccLoads[0]},
		{pinCCLoad2, &hw.ccLoads[1]},
		{pinCCLoad3, &hw.ccLoads[2]},
		{pinCCLoad4, &hw.ccLoads[3]},
		{pinCCLoad5, &hw.ccLoads[4]},
		{pinCCLoad6, &hw.ccLoads[5]},
	}
	for _, pd := range outputPinDefs {
		p := gpioreg.ByName(pd.name)
		if p == nil {
			return nil, fmt.Errorf("GPIO pin %s not found", pd.name)
		}
		if err := p.Out(gpio.Low); err != nil {
			return nil, fmt.Errorf("set %s as output: %v", pd.name, err)
		}
		*pd.pin = p
	}

	inputPinDefs := []struct {
		name string
		pin  *gpio.PinIn
	}{
		{pinBatterySenseDigital, &hw.batterySenseDigital},
		{pinAdcReady, &hw.adcReady},
	}
	for _, pd := range inputPinDefs {
		p := gpioreg.ByName(pd.name)
		if p == nil {
			return nil, fmt.Errorf("GPIO pin %s not found", pd.name)
		}
		if err := p.In(gpio.Float, gpio.NoEdge); err != nil {
			return nil, fmt.Errorf("set %s as input: %v", pd.name, err)
		}
		*pd.pin = p
	}

	hw.chargeMonitor = &INA219{dev: &i2c.Dev{Bus: bus, Addr: i2cAddrINA219Charge}, ShuntOhms: pcbShuntOhms}
	if err := hw.chargeMonitor.init(); err != nil {
		return nil, fmt.Errorf("init charge INA219: %v", err)
	}

	hw.dischargeMonitor = &INA219{dev: &i2c.Dev{Bus: bus, Addr: i2cAddrINA219Discharge}, ShuntOhms: pcbShuntOhms}
	if err := hw.dischargeMonitor.init(); err != nil {
		return nil, fmt.Errorf("init discharge INA219: %v", err)
	}

	hw.adc = &ADS1115{dev: &i2c.Dev{Bus: bus, Addr: i2cAddrADS1115}}

	return hw, nil
}

func (hw *hardware) close() {
	hw.setChargeEnable(false)
	hw.setShortCircuit(false)
	hw.setCCLoads(0)
	hw.setLED(false, false, false)
	hw.bus.Close()
}

func (hw *hardware) setChargeEnable(enable bool) error {
	if enable {
		log.Println("Enabling battery charge...")
	} else {
		log.Println("Disabling battery charge...")
	}
	return hw.chargeEn.Out(gpioLevel(enable))
}

func (hw *hardware) setShortCircuit(enable bool) error {
	return hw.enShort.Out(gpioLevel(enable))
}

func (hw *hardware) setFan(enable bool) error {
	return hw.enFan.Out(gpioLevel(enable))
}

func (hw *hardware) setLED(r, g, b bool) error {
	if err := hw.ledR.Out(gpioLevel(r)); err != nil {
		return err
	}
	if err := hw.ledG.Out(gpioLevel(g)); err != nil {
		return err
	}
	return hw.ledB.Out(gpioLevel(b))
}

// setCCLoads enables `count` randomly-selected CC loads (0–6) and controls the fan.
// Each load draws ~500mA; the fan runs whenever any loads are active.
func (hw *hardware) setCCLoads(count int) error {
	if count < 0 || count > 6 {
		return fmt.Errorf("invalid CC load count %d (must be 0-6)", count)
	}

	indices := []int{0, 1, 2, 3, 4, 5}
	rand.Shuffle(len(indices), func(i, j int) { indices[i], indices[j] = indices[j], indices[i] })
	enabled := make(map[int]bool, count)
	for _, idx := range indices[:count] {
		enabled[idx] = true
	}
	for i := range 6 {
		if err := hw.ccLoads[i].Out(gpioLevel(enabled[i])); err != nil {
			return fmt.Errorf("set CC load %d: %v", i+1, err)
		}
	}
	return hw.setFan(count > 0)
}

// withRetry calls fn up to n times, logging a warning on each transient failure.
func withRetry(name string, n int, fn func() error) error {
	var lastErr error
	for i := 0; i < n; i++ {
		if lastErr = fn(); lastErr == nil {
			return nil
		}
		if i < n-1 {
			log.Warnf("Failed reading %s (attempt %d/%d): %v", name, i+1, n, lastErr)
		}
	}
	return lastErr
}

// readChargeMonitor returns bus voltage (V) and shunt current (A) for the charge path.
func (hw *hardware) readChargeMonitor() (voltage, current float64, err error) {
	err = withRetry("charge monitor", 3, func() error {
		voltage, current, err = hw.chargeMonitor.read()
		return err
	})
	return
}

// readDischargeMonitor returns bus voltage (V) and shunt current (A) for the discharge path.
func (hw *hardware) readDischargeMonitor() (voltage, current float64, err error) {
	err = withRetry("discharge monitor", 3, func() error {
		voltage, current, err = hw.dischargeMonitor.read()
		return err
	})
	return
}

// readBatteryVoltage returns the battery pack voltage in volts via the ADS1115.
func (hw *hardware) readBatteryVoltage() (volts float64, err error) {
	err = withRetry("battery voltage", 3, func() error {
		raw, e := hw.adc.readChannel(0)
		log.Println(raw)
		if e != nil {
			return e
		}
		// volts = raw * battVoltDividerRatio This is what should be used but because of issue on PCB we need to use the below equation.
		volts = raw * battVoltDividerRatio * 11.77 / 8.87
		return nil
	})
	return
}

// NCP18XH103F03RB NTC thermistor: 10kΩ @ 25°C, B = 3380K.
// Table entries are (temperature °C, resistance kΩ), sorted ascending by temperature.
type ntcPoint struct{ tempC, resKOhm float64 }

var ntcTable = []ntcPoint{
	{-40, 195.652}, {-35, 148.171}, {-30, 113.347}, {-25, 87.559}, {-20, 68.237}, {-15, 53.650},
	{-10, 42.506}, {-5, 33.892}, {0, 27.219}, {5, 22.021}, {10, 17.926}, {15, 14.674},
	{20, 12.081}, {25, 10.000}, {30, 8.315}, {35, 6.948}, {40, 5.834}, {45, 4.917},
	{50, 4.161}, {55, 3.535}, {60, 3.014}, {65, 2.586}, {70, 2.228}, {75, 1.925},
	{80, 1.669}, {85, 1.452}, {90, 1.268}, {95, 1.110}, {100, 0.974}, {105, 0.858},
	{110, 0.758}, {115, 0.672}, {120, 0.596}, {125, 0.531},
}

const (
	ntcPullUpOhms = 10000.0 // R419 = 10kΩ pull-up to +3V3_Lin
	ntcVcc        = 3.3     // +3V3_Lin supply voltage
)

// voltageToTemperature converts an ADS1115 voltage (V) from the thermistor divider to °C.
// The thermistor is on the high side so we need to convert the voltage reading is of the 10k resistor
// and should be converted to the voltage of the thermistor.
func voltageToTemperature(v float64) (float64, error) {
	if v <= 0 || v >= ntcVcc {
		return 0, fmt.Errorf("temperature voltage %.3fV out of range (0–%.1fV)", v, ntcVcc)
	}
	// Convert voltage of 10k to voltage of thermistor
	v = 3.3 - v
	resKOhm := (ntcPullUpOhms / 1000.0) * v / (ntcVcc - v)
	// Table is sorted ascending by temp, descending by resistance.
	for i := 0; i < len(ntcTable)-1; i++ {
		hi := ntcTable[i]
		lo := ntcTable[i+1]
		if resKOhm <= hi.resKOhm && resKOhm >= lo.resKOhm {
			t := lo.tempC + (hi.tempC-lo.tempC)*(resKOhm-lo.resKOhm)/(hi.resKOhm-lo.resKOhm)
			return t, nil
		}
	}
	return 0, fmt.Errorf("thermistor resistance %.3fkΩ out of table range", resKOhm)
}

// readTemperature returns the temperature in °C from the NTC thermistor on J405.
func (hw *hardware) readTemperature() (tempC float64, err error) {
	err = withRetry("temperature", 3, func() error {
		v, e := hw.adc.readChannel(1)
		log.Println(v)
		if e != nil {
			return e
		}
		tempC, e = voltageToTemperature(v)
		return e
	})
	return
}

func gpioLevel(high bool) gpio.Level {
	if high {
		return gpio.High
	}
	return gpio.Low
}

func (hw *hardware) testCharge(battStateChan chan BatteryStatus) (bool, error) {
	log.Println("Waiting for battery status.")
	<-battStateChan

	// Enable battery charging
	if err := hw.setChargeEnable(true); err != nil {
		log.Printf("Error setting charge enable: %v", err)
	}
	defer func() {
		hw.setChargeEnable(false)
	}()

	// Get new status
	log.Println("Waiting for battery status.")
	batteryState := <-battStateChan
	log.Infof("Battery status: %+v", batteryState)

	const (
		notCharging chargingStatus = iota
		trickleCharge
		preCharge
		fastChargeCC
		taperChargeCV
		reserved
		topOffTimerActivatedCharging
		chargeTerminationDone
	)

	validChargeState := []chargingStatus{
		trickleCharge,
		preCharge,
		fastChargeCC,
		taperChargeCV,
		topOffTimerActivatedCharging,
		chargeTerminationDone,
	}

	// Check if valid charging state
	validChargingState := slices.Contains(validChargeState, batteryState.chargingStatus)
	if !validChargingState {
		log.Infof("Battery is not in a valid charging state: %s", batteryState.chargingStatus)
		return false, nil
	}

	// Check Power Good
	if !batteryState.powerGood {
		log.Info("Battery power is not good.")
		return false, nil
	}

	log.Info("Battery is in a valid charging state.")
	return true, nil
}

func (hw *hardware) testDischarge(battStateChan chan BatteryStatus) (bool, error) {
	log.Println("Waiting for battery status.")
	<-battStateChan

	hw.setCCLoads(2)
	defer hw.setCCLoads(0)

	// Get new status
	log.Println("Waiting for battery status.")
	batteryState := <-battStateChan
	log.Infof("Battery status: %+v", batteryState)

	voltage, current, err := hw.readDischargeMonitor()
	if err != nil {
		return false, fmt.Errorf("reading discharge monitor: %v", err)
	}
	log.Println(voltage, current)

	current = math.Abs(current)

	if current < 0.9 {
		log.Println("Current too low.")
		return false, nil
	}
	if current > 1.1 {
		log.Println("Current too high.")
		return false, nil
	}

	log.Println("Passed discharge test.")

	return true, nil
}

// overCurrentDischargeTest will return true if the battery passes the test.
func (hw *hardware) overCurrentDischargeTest(battStateChan chan BatteryStatus) (bool, error) {
	// TODO update firmware on the battery to send an updated battery status when a over discharge is detected

	log.Println("Waiting for battery status.")
	<-battStateChan

	// Status every 10 seconds so we wait 9 seconds after the last one to get a status quickly after triggering OCD
	time.Sleep(time.Second * 9)

	// Take battery voltage reading before triggering discharge.
	batteryVoltage, err := hw.readBatteryVoltage()
	if err != nil {
		return false, fmt.Errorf("reading battery voltage: %v", err)
	}

	// Trigger over current discharge.
	log.Info("Triggering over current discharge.")
	hw.setCCLoads(6)
	defer hw.setCCLoads(0)
	time.Sleep(200 * time.Millisecond)

	// Take battery voltage reading during OCD
	ocdBatteryVoltage, err := hw.readBatteryVoltage()
	if err != nil {
		return false, fmt.Errorf("reading battery voltage: %v", err)
	}
	hw.setCCLoads(0)

	log.Info("Waiting for battery status.")
	batteryStatus := <-battStateChan

	log.Info("Waiting for battery voltage to recover.")
	recovered := false
	for range 20 {
		time.Sleep(time.Second)
		batV, err := hw.readBatteryVoltage()
		if err != nil {
			return false, fmt.Errorf("reading battery voltage: %v", err)
		}
		if batV > 9 {
			recovered = true
			break
		}
	}
	if !recovered {
		log.Println("Battery voltage did not recover.")
		return false, nil
	}

	log.Infof("Battery Status: %s", batteryStatus)
	log.Infof("Battery Voltage Before: %v", batteryVoltage)
	log.Infof("Battery Voltage During OCD: %v", ocdBatteryVoltage)

	return true, nil
}

func (hw *hardware) testShortCircuit(battStateChan chan BatteryStatus) (bool, error) {
	log.Println("Waiting for battery status.")
	<-battStateChan

	// Waiting just before next reading to make a short.
	time.Sleep(time.Second * 9)

	// Shorting battery
	log.Info("Triggering short circuit...")
	if err := hw.setShortCircuit(true); err != nil {
		return false, fmt.Errorf("setting short circuit: %v", err)
	}
	defer hw.setShortCircuit(false)
	time.Sleep(100 * time.Millisecond)

	voltage, current, err := hw.readDischargeMonitor()
	if err != nil {
		return false, fmt.Errorf("reading discharge monitor: %v", err)
	}
	log.Println(voltage, current)
	if voltage > 0.1 {
		log.Println("Voltage too high.")
		return false, nil
	}

	// Disable short and wait for battery voltage to recover.
	if err := hw.setShortCircuit(false); err != nil {
		return false, fmt.Errorf("setting short circuit: %v", err)
	}

	// Enable charge to enable battery pack again.
	if err := hw.setChargeEnable(true); err != nil {
		return false, fmt.Errorf("setting charge enable: %v", err)
	}
	time.Sleep(10 * time.Second)

	log.Info("Waiting for battery voltage to recover.")
	recovered := false
	for range 20 {
		time.Sleep(time.Second)
		batV, err := hw.readBatteryVoltage()
		if err != nil {
			return false, fmt.Errorf("reading battery voltage: %v", err)
		}
		if batV > 9 {
			recovered = true
			break
		}
	}
	if !recovered {
		log.Println("Battery voltage did not recover.")
		return false, nil
	}

	// TODO In firmware make flag in state so we can check if short circuit triggered in last minute.

	return true, nil
}

func (hw *hardware) runChargeSeq(battStateChan chan BatteryStatus) error {
	if err := hw.setChargeEnable(true); err != nil {
		return fmt.Errorf("setting charge enable: %v", err)
	}

	csvFilename := fmt.Sprintf("charge_%s.csv", time.Now().Format("2006-01-02_15-04-05"))
	f, err := os.Create(csvFilename)
	if err != nil {
		return fmt.Errorf("creating CSV file: %v", err)
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()

	header := []string{
		"timestamp",
		"voltage_V", "current_A",
		"tempAHT_C", "tempBQ76920_C", "tempBQ25798_C",
		"cell1_mV", "cell2_mV", "cell3_mV",
		"vbus_mV", "vbat_mV",
		"ibus_mA", "ibat_mA", "ibatCC_mA",
		"chargingStatus",
	}
	if err := w.Write(header); err != nil {
		return fmt.Errorf("writing CSV header: %v", err)
	}

	fmtF := func(f float64) string { return strconv.FormatFloat(f, 'f', 3, 64) }
	fmtI := func(i int) string { return strconv.Itoa(i) }

	timeout := time.After(12 * time.Hour)
	for {
		select {
		case <-timeout:
			return fmt.Errorf("charge sequence timed out after 12 hours")
		case state := <-battStateChan:
			voltage, current, err := hw.readChargeMonitor()
			if err != nil {
				log.Warnf("Reading charge monitor: %v", err)
				continue
			}

			tempAHT := float64(state.TempAHTdC) / 10
			tempBQ76920 := float64(state.TempBQ76920dC) / 10
			tempBQ25798 := float64(state.TempBQ25798dC) / 10

			log.Infof(
				"voltage=%.3fV current=%.3fA | tempAHT=%.1f°C tempBQ76920=%.1f°C tempBQ25798=%.1f°C | "+
					"cell1=%dmV cell2=%dmV cell3=%dmV vbus=%dmV vbat=%dmV | "+
					"ibus=%dmA ibat=%dmA ibatCC=%dmA | chargingStatus=%s",
				voltage, current,
				tempAHT, tempBQ76920, tempBQ25798,
				state.Cell1mV, state.Cell2mV, state.Cell3mV, state.VbusmV, state.VbatmV,
				state.IbusmA, state.IbatmA, state.IbatCCmA,
				state.chargingStatus,
			)

			row := []string{
				time.Now().Format(time.RFC3339),
				fmtF(voltage), fmtF(current),
				fmtF(tempAHT), fmtF(tempBQ76920), fmtF(tempBQ25798),
				fmtI(int(state.Cell1mV)), fmtI(int(state.Cell2mV)), fmtI(int(state.Cell3mV)),
				fmtI(int(state.VbusmV)), fmtI(int(state.VbatmV)),
				fmtI(int(state.IbusmA)), fmtI(int(state.IbatmA)), fmtI(int(state.IbatCCmA)),
				state.chargingStatus.String(),
			}
			if err := w.Write(row); err != nil {
				log.Warnf("Writing CSV row: %v", err)
			}
			w.Flush()

			if state.chargingStatus == chargeTerminationDone {
				log.Info("Charge sequence complete.")
				return nil
			}
		}
	}
}
