//go:build aix

/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// On AIX a PowerVS volume shows up as an MPIO disk, hdiskN, once cfgmgr has
// configured it; AIX's MPIO driver collapses its paths into that one device,
// so there is no device-mapper layer. The volume is found by its WWN, which
// is embedded in the disk's unique_id ODM attribute.
//
// Device.Mapper is /dev/hdiskN and Device.Slaves is the number of Enabled
// MPIO paths. DeleteDevice exports the volume's volume group (see
// pkg/aixlvm) and then removes the disk with rmdev -dl, so the paths are
// gone before the controller detaches the volume.

package device

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"k8s.io/klog/v2"

	"sigs.k8s.io/ibm-powervs-block-csi-driver/pkg/aixlvm"
)

// LinuxDevice is the device interface the node service uses. The name is
// kept from the Linux implementation; on AIX the device is an hdisk.
type LinuxDevice interface {
	DeleteDevice() (err error)
	CreateDevice() (err error)
	GetMapper() string
	Populate(bool) error
}

// Device is a PowerVS volume's disk.
type Device struct {
	Mapper string `json:"mapper,omitempty"`
	WWN    string `json:"wwn,omitempty"`
	Slaves int    `json:"slaves,omitempty"`
}

var (
	scanLock = &sync.Mutex{}
	// run runs the query commands and cfgmgr. They get a timeout; the
	// commands that change LVM state run through aixlvm without one.
	run aixlvm.Runner = aixlvm.ExecRunner{Timeout: 10 * time.Minute}
)

// NewLinuxDevice returns the device for a WWN.
func NewLinuxDevice(wwn string) LinuxDevice {
	return &Device{WWN: wwn}
}

// GetMapper returns the device path, /dev/hdiskN.
func (d *Device) GetMapper() string {
	return d.Mapper
}

// Populate looks for the disk with this WWN among the configured disks. It
// sets Mapper and Slaves when it finds one with at least one Enabled path.
// A matching disk with no Enabled path is a leftover of an earlier
// attachment: it is skipped, and removed if it is merely Defined and in no
// volume group.
func (d *Device) Populate(_ bool) error {
	ctx := context.Background()
	disks, err := listDisks(ctx)
	if err != nil {
		return err
	}
	for _, dk := range disks {
		if !uniqueIDMatchesWWN(dk.UniqueID, d.WWN) {
			continue
		}
		paths := enabledPaths(ctx, dk)
		if paths == 0 {
			klog.Warningf("disk %s for wwn %s has no enabled paths (status %s)", dk.Name, d.WWN, dk.Status)
			if dk.Status == "Defined" {
				removeStaleDisk(ctx, dk.Name)
			}
			continue
		}
		d.Mapper = "/dev/" + dk.Name
		d.Slaves = paths
		return nil
	}
	return nil
}

// CreateDevice configures newly attached disks and waits for this volume's
// disk to appear with an Enabled path.
func (d *Device) CreateDevice() error {
	for i := 0; i <= 10; i++ {
		if err := rescanWithLock(); err != nil {
			return err
		}
		time.Sleep(time.Second)
		if err := d.Populate(true); err != nil {
			return err
		}
		if d.Slaves > 0 {
			return nil
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("disk not found for wwn %s", d.WWN)
}

// DeleteDevice exports the volume group on the disk and removes the disk.
// A disk in a volume group the driver did not create is refused.
func (d *Device) DeleteDevice() error {
	if err := ReleaseByPath(d.Mapper); err != nil {
		return err
	}
	d.Mapper, d.Slaves = "", 0
	return nil
}

// ReleaseByPath exports the volume group behind a device path (the disk, or
// the logical volume the mount table shows) and removes the disk with
// rmdev -dl. Unlike Populate it does not need the disk to have an Enabled
// path, so a volume whose paths are already gone is still cleaned up.
func ReleaseByPath(pathName string) error {
	ctx := context.Background()
	disk, err := diskOfPath(ctx, pathName)
	if err != nil {
		return err
	}
	for try := 0; ; try++ {
		err = aixlvm.Default.ReleaseAndRemove(ctx, disk)
		if err == nil || try == 2 {
			break
		}
		time.Sleep(5 * time.Second)
	}
	if err != nil {
		return fmt.Errorf("releasing %s: %w", disk, err)
	}
	return nil
}

// diskOfPath maps /dev/hdiskN, /dev/rhdiskN or /dev/<lv> to the disk name.
func diskOfPath(ctx context.Context, pathName string) (string, error) {
	name := strings.TrimPrefix(pathName, "/dev/")
	if strings.HasPrefix(name, "r") && aixDiskNameRe.MatchString(name[1:]) {
		name = name[1:]
	}
	if aixDiskNameRe.MatchString(name) {
		return name, nil
	}
	disk, err := aixlvm.Default.DiskOfLV(ctx, name)
	if err != nil {
		return "", fmt.Errorf("finding the disk for %s: %w", pathName, err)
	}
	return disk, nil
}

// GetDeviceWWN returns the WWN of the volume behind a device path: a disk
// (/dev/hdiskN or /dev/rhdiskN) or the logical volume a JFS/JFS2 file system
// is mounted from, as the mount table shows it.
func GetDeviceWWN(pathName string) (string, error) {
	ctx := context.Background()
	name, err := diskOfPath(ctx, pathName)
	if err != nil {
		return "", err
	}
	out, err := run.Run(ctx, "odmget", "-q", "name="+name+" and attribute=unique_id", "CuAt")
	if err != nil {
		return "", err
	}
	uid := parseODMAttr(string(out))[name]
	wwn := wwnFromUniqueID(uid)
	if wwn == "" {
		return "", fmt.Errorf("no WWN in unique_id %q of %s", uid, name)
	}
	return wwn, nil
}

func listDisks(ctx context.Context) ([]aixDisk, error) {
	out, err := run.Run(ctx, "odmget", "-q", "attribute=unique_id", "CuAt")
	if err != nil {
		return nil, fmt.Errorf("reading disk unique_ids: %w", err)
	}
	ids := parseODMAttr(string(out))
	out, err = run.Run(ctx, "lsdev", "-Cc", "disk", "-F", "name:status")
	if err != nil {
		return nil, fmt.Errorf("listing disks: %w", err)
	}
	var disks []aixDisk
	for name, st := range parseDiskStatus(string(out)) {
		disks = append(disks, aixDisk{Name: name, Status: st, UniqueID: ids[name]})
	}
	return disks, nil
}

// enabledPaths counts a disk's Enabled MPIO paths. A disk lspath does not
// know (not MPIO-managed) counts as one path when it is Available.
func enabledPaths(ctx context.Context, dk aixDisk) int {
	out, err := run.Run(ctx, "lspath", "-l", dk.Name, "-F", "status")
	if err != nil {
		if dk.Status == "Available" {
			return 1
		}
		return 0
	}
	return countEnabledPaths(string(out))
}

// removeStaleDisk removes a Defined disk left by an earlier attachment, if no
// volume group on this node uses it. Best effort.
func removeStaleDisk(ctx context.Context, disk string) {
	out, err := run.Run(ctx, "lspv")
	if err != nil {
		return
	}
	for _, pv := range aixlvm.ParseLspv(string(out)) {
		if pv.Name == disk && pv.VG != aixlvm.NoVG {
			return
		}
	}
	aixlvm.Default.Lock()
	defer aixlvm.Default.Unlock()
	if _, err := run.Run(ctx, "rmdev", "-dl", disk); err != nil {
		klog.Warningf("removing stale disk %s: %v", disk, err)
	}
}

// rescanWithLock runs cfgmgr, one scan at a time. A caller that finds a scan
// already running waits for it and does not scan again, as on Linux.
func rescanWithLock() error {
	start := time.Now()
	scan := true
	for {
		if scanLock.TryLock() {
			defer scanLock.Unlock()
			if scan {
				return rescan(context.Background())
			}
			return nil
		}
		if time.Since(start) > 2*time.Minute {
			return nil
		}
		scan = false
		time.Sleep(5 * time.Second)
	}
}

// rescan configures new disks behind the Fibre Channel and virtual SCSI
// adapters, or runs a full cfgmgr if there are none. It holds the LVM lock,
// so disks are not configured or renumbered while a volume group is being
// made, imported or exported.
func rescan(ctx context.Context) error {
	aixlvm.Default.Lock()
	defer aixlvm.Default.Unlock()
	out, err := run.Run(ctx, "lsdev", "-Cc", "adapter", "-F", "name")
	if err != nil {
		return err
	}
	adapters := parseScanAdapters(string(out))
	if len(adapters) == 0 {
		_, err := run.Run(ctx, "cfgmgr")
		return err
	}
	for _, a := range adapters {
		if _, err := run.Run(ctx, "cfgmgr", "-l", a); err != nil {
			// One adapter failing (for example an unused port) should not
			// stop the others from being scanned.
			klog.Warningf("cfgmgr -l %s: %v", a, err)
		}
	}
	return nil
}
