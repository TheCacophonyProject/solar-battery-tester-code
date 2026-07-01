package main

import (
	"encoding/binary"
	"fmt"
	"strings"
	"time"

	"github.com/tarm/serial"
)

// BatteryStatus holds the parsed 0x90 periodic status message from the ATtiny1616 battery manager.
// Sent every 10 seconds at 9600 baud. Layout matches main.cpp serial writes (36 bytes, all
// little-endian, no struct padding — identical to the Python struct "<IhhhBHHHHHHhh5s4s").
type BatteryStatus struct {
	Seconds                  uint32 // time since boot (s)
	TempAHTdC                int16  // AHT20 temperature (°C × 10)
	TempBQ76920dC            int16  // BQ76920 NTC temperature (°C × 10)
	TempBQ25798dC            int16  // BQ25798 NTC temperature (°C × 10)
	HumidityPct              uint8  // AHT20 relative humidity (%)
	Cell1mV                  uint16
	Cell2mV                  uint16
	Cell3mV                  uint16
	VbusmV                   uint16  // input bus voltage (mV)
	IbusmA                   uint16  // input bus current (mA)
	VbatmV                   uint16  // battery pack voltage (mV)
	IbatmA                   int16   // battery current: positive = charging, negative = discharging (mA)
	IbatCCmA                 int16   // BQ76920 coulomb-counter current (mA)
	ChgStat                  [5]byte // BQ25798 REG1B..REG1F (STATUS_0..4)
	BQStat                   [4]byte // BQ76920: SYS_STAT, CELLBAL1, SYS_CTRL1, SYS_CTRL2
	chargingStatus           chargingStatus
	vbusStatus               vbusStatus
	powerGood                bool
	chargerThermalRegulation bool
	chargerVBatPresent       bool
	scd                      bool
	ocd                      bool
}

type chargingStatus int

// In the datasheet look at the graph 9-10 to see the different charge states described there.
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

func (c chargingStatus) String() string {
	switch c {
	case notCharging:
		return "notCharging"
	case trickleCharge:
		return "trickleCharge"
	case preCharge:
		return "preCharge"
	case fastChargeCC:
		return "fastChargeCC"
	case taperChargeCV:
		return "taperChargeCV"
	case reserved:
		return "reserved"
	case topOffTimerActivatedCharging:
		return "topOffTimerActivatedCharging"
	case chargeTerminationDone:
		return "chargeTerminationDone"
	default:
		return "unknown"
	}
}

type vbusStatus int

const (
	vbusNoInput vbusStatus = iota
	vbusUSBSDP
	vbusUSBCDP
	vbusUSBDCP
	vbusHVDCP
	vbusUnknownAdaptor
	vbusNonStandardAdaptor
	vbusOTG
	vbusNotQualifiedAdaptor
	vbusReserved9
	vbusReservedA
	vbusDirectFromVBUS
	vbusBackupMode
	vbusReservedD
	vbusReservedE
	vbusReservedF
)

func (v vbusStatus) String() string {
	switch v {
	case vbusNoInput:
		return "noInput"
	case vbusUSBSDP:
		return "usbSDP(500mA)"
	case vbusUSBCDP:
		return "usbCDP(1.5A)"
	case vbusUSBDCP:
		return "usbDCP(3.25A)"
	case vbusHVDCP:
		return "hvdcp(1.5A)"
	case vbusUnknownAdaptor:
		return "unknownAdaptor(3A)"
	case vbusNonStandardAdaptor:
		return "nonStandardAdaptor"
	case vbusOTG:
		return "otg"
	case vbusNotQualifiedAdaptor:
		return "notQualifiedAdaptor"
	case vbusDirectFromVBUS:
		return "directFromVBUS"
	case vbusBackupMode:
		return "backupMode"
	default:
		return "reserved"
	}
}

func (s *BatteryStatus) TempAHT() float64     { return float64(s.TempAHTdC) / 10.0 }
func (s *BatteryStatus) TempBQ76920() float64 { return float64(s.TempBQ76920dC) / 10.0 }
func (s *BatteryStatus) TempBQ25798() float64 { return float64(s.TempBQ25798dC) / 10.0 }
func (s *BatteryStatus) IsCharging() bool     { return s.IbatmA > 0 }

// BQFaults returns BQ76920 SYS_STAT fault flags as human-readable strings.
func (s *BatteryStatus) BQFaults() []string {
	stat := s.BQStat[0]
	var faults []string
	if stat&0x08 != 0 {
		faults = append(faults, "UV")
	}
	if stat&0x04 != 0 {
		faults = append(faults, "OV")
	}
	if stat&0x02 != 0 {
		faults = append(faults, "SCD")
	}
	if stat&0x01 != 0 {
		faults = append(faults, "OCD")
	}
	return faults
}

const (
	batteryBaudRate         = 9600
	batteryStatusCode       = 0x90
	batteryStatusPayloadLen = 36 // bytes following the 0x90 code byte
)

// runBatteryMonitor reads and prints battery status messages in a loop until interrupted,
// writing each parsed status into ch.
func runBatteryMonitor(serialDev string, ch chan<- BatteryStatus) error {
	log.Printf("Monitoring battery UART on %s at %d baud", serialDev, batteryBaudRate)
	port, err := serial.OpenPort(&serial.Config{
		Name:        serialDev,
		Baud:        batteryBaudRate,
		ReadTimeout: time.Second,
	})
	if err != nil {
		return fmt.Errorf("open %s: %v", serialDev, err)
	}
	defer port.Close()

	for {
		s, err := readBatteryStatus(port, 30*time.Second)
		if err != nil {
			log.Warnf("Read error: %v", err)
			continue
		}
		select {
		case ch <- *s:
		default:
			log.Warnf("Battery status channel full, dropping message")
		}
		log.Debugf("Battery status: %s", s)
	}
}

func (s BatteryStatus) String() string {
	faults := s.BQFaults()
	faultStr := "OK"
	if len(faults) > 0 {
		faultStr = strings.Join(faults, " ")
	}
	direction := "idle"
	if s.IsCharging() {
		direction = "charging"
	} else if s.IbatmA < 0 {
		direction = "discharging"
	}
	duration := time.Duration(s.Seconds) * time.Second
	return fmt.Sprintf(
		"t=%s  temp: aht=%.1f°C bq76920=%.1f°C bq25798=%.1f°C  hum=%d%%\n"+
			"         cells: %dmV %dmV %dmV  vbus=%dmV ibus=%dmA\n"+
			"         vbat=%dmV ibat=%dmA(%s) ibat_cc=%dmA  bq=%s\n"+
			"         chargingStatus=%s vbusStatus=%s PG=%t TR=%t, VBAT=%t"+
			"         scd=%t ocd=%t",
		duration,
		s.TempAHT(), s.TempBQ76920(), s.TempBQ25798(), s.HumidityPct,
		s.Cell1mV, s.Cell2mV, s.Cell3mV, s.VbusmV, s.IbusmA,
		s.VbatmV, s.IbatmA, direction, s.IbatCCmA, faultStr,
		s.chargingStatus, s.vbusStatus, s.powerGood, s.chargerThermalRegulation, s.chargerVBatPresent,
		s.scd, s.ocd,
	)
}

// readBatteryStatus reads one 0x90 periodic status message from an open serial port.
func readBatteryStatus(port *serial.Port, timeout time.Duration) (*BatteryStatus, error) {
	deadline := time.Now().Add(timeout)
	buf := make([]byte, 1)
	for time.Now().Before(deadline) {
		n, err := port.Read(buf)
		if err != nil || n == 0 {
			continue
		}
		if buf[0] != batteryStatusCode {
			continue
		}

		payload := make([]byte, batteryStatusPayloadLen)
		total := 0
		for total < batteryStatusPayloadLen {
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("timeout reading payload (%d/%d bytes)", total, batteryStatusPayloadLen)
			}
			n, err := port.Read(payload[total:])
			if err != nil {
				return nil, fmt.Errorf("reading payload: %v", err)
			}
			total += n
		}
		return parseStatusPayload(payload)
	}
	return nil, fmt.Errorf("timeout waiting for battery 0x90 status message")
}

// parseStatusPayload decodes the 36-byte 0x90 payload into a BatteryStatus.
// All fields are little-endian with no padding (matches Python struct "<IhhhBHHHHHHhh5s4s").
func parseStatusPayload(d []byte) (*BatteryStatus, error) {
	if len(d) < batteryStatusPayloadLen {
		return nil, fmt.Errorf("payload too short: %d bytes", len(d))
	}
	s := &BatteryStatus{}
	s.Seconds = binary.LittleEndian.Uint32(d[0:4])
	s.TempAHTdC = int16(binary.LittleEndian.Uint16(d[4:6]))
	s.TempBQ76920dC = int16(binary.LittleEndian.Uint16(d[6:8]))
	s.TempBQ25798dC = int16(binary.LittleEndian.Uint16(d[8:10]))
	s.HumidityPct = d[10]
	s.Cell1mV = binary.LittleEndian.Uint16(d[11:13])
	s.Cell2mV = binary.LittleEndian.Uint16(d[13:15])
	s.Cell3mV = binary.LittleEndian.Uint16(d[15:17])
	s.VbusmV = binary.LittleEndian.Uint16(d[17:19])
	s.IbusmA = binary.LittleEndian.Uint16(d[19:21])
	s.VbatmV = binary.LittleEndian.Uint16(d[21:23])
	s.IbatmA = int16(binary.LittleEndian.Uint16(d[23:25]))
	s.IbatCCmA = int16(binary.LittleEndian.Uint16(d[25:27]))
	copy(s.ChgStat[:], d[27:32])
	copy(s.BQStat[:], d[32:36])
	s.chargingStatus = chargingStatus(d[28] >> 5)
	s.vbusStatus = vbusStatus((d[28] >> 1) & 0x0F)
	s.powerGood = bitHigh(d[27], 3)
	s.chargerThermalRegulation = bitHigh(d[29], 2)
	s.chargerVBatPresent = bitHigh(d[29], 0)
	s.scd = bitHigh(d[32], 1)
	s.ocd = bitHigh(d[32], 0)

	return s, nil
}

func bitHigh(b byte, i int) bool { return ((b >> i) & 1) == 1 }
