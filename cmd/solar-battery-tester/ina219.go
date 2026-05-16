package main

import (
	"encoding/binary"
	"fmt"

	"periph.io/x/conn/v3/i2c"
)

// INA219 register addresses.
const (
	ina219RegConfig  = 0x00
	ina219RegShunt   = 0x01
	ina219RegBus     = 0x02
	ina219RegPower   = 0x03
	ina219RegCurrent = 0x04
	ina219RegCalib   = 0x05
)

// INA219 config: BRNG=32V, PGA=±320mV, BADC=12-bit, SADC=12-bit, continuous shunt+bus.
// This is the device default (0x399F).
const ina219DefaultConfig = 0x399F

// INA219 shunt voltage LSB with PGA=±320mV: 10µV per bit.
const ina219ShuntVoltageLSB = 10e-6

// INA219 bus voltage LSB: 4mV per bit.
const ina219BusVoltageLSB = 4e-3

// INA219 wraps an INA219 current/voltage sensor.
type INA219 struct {
	dev       *i2c.Dev
	ShuntOhms float64
}

func (s *INA219) init() error {
	return s.writeReg(ina219RegConfig, ina219DefaultConfig)
}

// read returns bus voltage (V) and shunt current (A).
func (s *INA219) read() (voltage, current float64, err error) {
	shuntRaw, err := s.readReg(ina219RegShunt)
	if err != nil {
		return 0, 0, fmt.Errorf("read shunt voltage: %v", err)
	}
	busRaw, err := s.readReg(ina219RegBus)
	if err != nil {
		return 0, 0, fmt.Errorf("read bus voltage: %v", err)
	}

	// Shunt voltage is a signed 16-bit value.
	shuntV := float64(int16(shuntRaw)) * ina219ShuntVoltageLSB

	// Bus voltage: bits 15:3 are the value, shift right by 3.
	busV := float64(busRaw>>3) * ina219BusVoltageLSB

	current = shuntV / s.ShuntOhms
	voltage = busV
	return voltage, current, nil
}

func (s *INA219) writeReg(reg uint8, value uint16) error {
	buf := []byte{reg, byte(value >> 8), byte(value & 0xFF)}
	return s.dev.Tx(buf, nil)
}

func (s *INA219) readReg(reg uint8) (uint16, error) {
	buf := make([]byte, 2)
	if err := s.dev.Tx([]byte{reg}, buf); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(buf), nil
}
