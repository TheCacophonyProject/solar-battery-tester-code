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

// waitForUSBDrive polls for a removable USB mass-storage device and mounts it
// at usbMountPoint, returning once it's mounted and confirmed writable.
//
// Drives get pulled out between test runs (to copy data off) and a different
// one plugged back in, so on each call this also clears out any mount left
// over from a drive that's no longer the one currently plugged in — whether
// because it was properly unmounted by unmountUSBDrive, or yanked without
// unmounting first (which otherwise leaves a dead mount pointing at a device
// that no longer exists).
func waitForUSBDrive() (string, error) {
	for {
		dev, err := findUSBPartition()
		if err != nil {
			return "", err
		}
		if dev == "" {
			time.Sleep(usbPollInterval)
			continue
		}

		if err := os.MkdirAll(usbMountPoint, 0o755); err != nil {
			return "", fmt.Errorf("creating USB mount point: %v", err)
		}

		mountedDev, mounted := currentMountSource(usbMountPoint)
		if mounted && mountedDev != dev {
			log.Warnf("Stale mount at %s (was %s, now %s present) — clearing it", usbMountPoint, mountedDev, dev)
			if out, err := exec.Command("umount", "-l", usbMountPoint).CombinedOutput(); err != nil {
				log.Warnf("Failed to clear stale mount: %v (%s)", err, strings.TrimSpace(string(out)))
			}
			mounted = false
		}

		if !mounted {
			if out, err := exec.Command("mount", dev, usbMountPoint).CombinedOutput(); err != nil {
				log.Warnf("Failed to mount %s at %s: %v (%s)", dev, usbMountPoint, err, strings.TrimSpace(string(out)))
				time.Sleep(usbPollInterval)
				continue
			}
		}

		if err := checkWritable(usbMountPoint); err != nil {
			log.Warnf("USB drive at %s is not writable, will retry: %v", usbMountPoint, err)
			exec.Command("umount", "-l", usbMountPoint).Run()
			time.Sleep(usbPollInterval)
			continue
		}

		return usbMountPoint, nil
	}
}

// checkWritable confirms path can actually be written to by creating and removing a temp file in it.
func checkWritable(path string) error {
	f, err := os.CreateTemp(path, ".write-test-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

// unmountUSBDrive flushes and unmounts the drive at usbMountPoint so it's safe to
// physically remove. Safe to call even if nothing is currently mounted there.
func unmountUSBDrive() error {
	if _, mounted := currentMountSource(usbMountPoint); !mounted {
		return nil
	}
	exec.Command("sync").Run()
	if out, err := exec.Command("umount", usbMountPoint).CombinedOutput(); err != nil {
		return fmt.Errorf("unmounting %s: %v (%s)", usbMountPoint, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// waitForUSBRemoval blocks until no removable USB disk is present, so the rest
// of the program doesn't race ahead of the user physically pulling the drive.
func waitForUSBRemoval() error {
	for {
		dev, err := findUSBPartition()
		if err != nil {
			return err
		}
		if dev == "" {
			return nil
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

// currentMountSource returns the device currently mounted at path, if any.
func currentMountSource(path string) (dev string, mounted bool) {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 1 && fields[1] == path {
			return fields[0], true
		}
	}
	return "", false
}
