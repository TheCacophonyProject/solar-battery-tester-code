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
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	usbMountPoint   = "/mnt/solar-battery-tester-usb"
	usbPollInterval = 2 * time.Second
)

// waitForUSBDrive polls for a removable USB mass-storage device, mounts its
// first partition (or the whole disk if unpartitioned) at usbMountPoint, and
// returns the mount point. It blocks until a drive is found or ctx-like
// cancellation isn't needed here since this only runs at startup.
func waitForUSBDrive() (string, error) {
	for {
		dev, err := findUSBPartition()
		if err != nil {
			return "", err
		}
		if dev != "" {
			if err := os.MkdirAll(usbMountPoint, 0o755); err != nil {
				return "", fmt.Errorf("creating USB mount point: %v", err)
			}
			if !isMounted(usbMountPoint) {
				if out, err := exec.Command("mount", dev, usbMountPoint).CombinedOutput(); err != nil {
					log.Warnf("Failed to mount %s at %s: %v (%s)", dev, usbMountPoint, err, strings.TrimSpace(string(out)))
					time.Sleep(usbPollInterval)
					continue
				}
			}
			return usbMountPoint, nil
		}
		time.Sleep(usbPollInterval)
	}
}

// findUSBPartition looks under /sys/block for a removable disk (e.g. sda)
// and returns the device path of its first partition, or of the whole disk
// if it has no partition table. Returns "" if no removable disk is present.
func findUSBPartition() (string, error) {
	entries, err := os.ReadDir("/sys/block")
	if err != nil {
		return "", fmt.Errorf("reading /sys/block: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "sd") {
			continue
		}
		removable, err := os.ReadFile(filepath.Join("/sys/block", name, "removable"))
		if err != nil || strings.TrimSpace(string(removable)) != "1" {
			continue
		}

		diskEntries, err := os.ReadDir(filepath.Join("/sys/block", name))
		if err != nil {
			continue
		}
		for _, de := range diskEntries {
			if strings.HasPrefix(de.Name(), name) && de.Name() != name {
				return filepath.Join("/dev", de.Name()), nil
			}
		}
		// No partitions found; use the whole disk.
		return filepath.Join("/dev", name), nil
	}
	return "", nil
}

// isMounted reports whether path is currently a mount point.
func isMounted(path string) bool {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 1 && fields[1] == path {
			return true
		}
	}
	return false
}
