package main

import (
	"encoding/binary"
	"fmt"
	"time"

	"periph.io/x/conn/v3/i2c"
)

// ADS1115 register addresses.
const (
	ads1115RegConversion = 0x00
	ads1115RegConfig     = 0x01
)

// ADS1115 config bits.
const (
	ads1115OsStart     = 0x8000 // Start single conversion
	ads1115ModeSingle  = 0x0100 // Single-shot mode
	ads1115Pga4096mV   = 0x0200 // PGA = ±4.096V (LSB = 125µV)
	ads1115Rate128sps  = 0x0080 // 128 samples per second
	ads1115CompDisable = 0x0003 // Disable comparator

	// MUX: single-ended channel selections (AINx vs GND).
	ads1115MuxAIN0 = 0x4000
	ads1115MuxAIN1 = 0x5000
	ads1115MuxAIN2 = 0x6000
	ads1115MuxAIN3 = 0x7000

	// Voltage per LSB for ±4.096V FSR with 16-bit signed result.
	ads1115VoltsPerLSB = 4.096 / 32768.0
)

var ads1115MuxByChannel = [4]uint16{
	ads1115MuxAIN0,
	ads1115MuxAIN1,
	ads1115MuxAIN2,
	ads1115MuxAIN3,
}

// ADS1115 wraps an ADS1115 16-bit ADC.
type ADS1115 struct {
	dev *i2c.Dev
}

// readChannel reads a single-ended voltage (V) on channel 0–3.
func (a *ADS1115) readChannel(ch int) (float64, error) {
	if ch < 0 || ch > 3 {
		return 0, fmt.Errorf("invalid ADS1115 channel %d", ch)
	}
	mux := ads1115MuxByChannel[ch]
	config := ads1115OsStart | mux | ads1115Pga4096mV | ads1115ModeSingle | ads1115Rate128sps | ads1115CompDisable
	if err := a.writeReg(ads1115RegConfig, config); err != nil {
		return 0, fmt.Errorf("start conversion: %v", err)
	}

	// Wait for conversion to complete (~8ms at 128 SPS).
	time.Sleep(10 * time.Millisecond)

	// Poll OS bit until set (conversion done).
	for range 10 {
		cfg, err := a.readReg(ads1115RegConfig)
		if err != nil {
			return 0, fmt.Errorf("read config: %v", err)
		}
		if cfg&ads1115OsStart != 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	raw, err := a.readReg(ads1115RegConversion)
	if err != nil {
		return 0, fmt.Errorf("read conversion: %v", err)
	}
	return float64(int16(raw)) * ads1115VoltsPerLSB, nil
}

func (a *ADS1115) writeReg(reg uint8, value uint16) error {
	buf := []byte{reg, byte(value >> 8), byte(value & 0xFF)}
	return a.dev.Tx(buf, nil)
}

func (a *ADS1115) readReg(reg uint8) (uint16, error) {
	buf := make([]byte, 2)
	if err := a.dev.Tx([]byte{reg}, buf); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(buf), nil
}
