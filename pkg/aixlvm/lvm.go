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

package aixlvm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"k8s.io/klog/v2"
)

const (
	// FSTypeJFS and FSTypeJFS2 are the AIX file systems the driver makes.
	FSTypeJFS  = "jfs"
	FSTypeJFS2 = "jfs2"

	// vgPrefix and lvPrefix start the names given to a volume's volume group
	// and logical volume. AIX allows 15 characters for both.
	vgPrefix = "pvs"
	lvPrefix = "pvl"
	// nameSuffixLen hex digits of the WWN's SHA-256 follow the prefix. A
	// hash rather than the WWN's own digits: IBM storage WWNs put the
	// controller's ID in the middle and a small sequence number at the end,
	// so the trailing digits of volumes from two controllers can be equal.
	nameSuffixLen = 12

	commandTimeout = 5 * time.Minute
)

var (
	ownedVGRe = regexp.MustCompile(`^` + vgPrefix + `[0-9a-f]{` + fmt.Sprint(nameSuffixLen) + `}$`)
	hexRe     = regexp.MustCompile(`^[0-9a-fA-F]+$`)
	diskRe    = regexp.MustCompile(`^hdisk[0-9]+$`)
	lvNameRe  = regexp.MustCompile(`^[A-Za-z0-9_.]{1,15}$`)
)

// ErrNotBlank is returned when a disk with no volume group holds data.
var ErrNotBlank = errors.New("disk is not blank")

// ErrInUse is returned when a disk belongs to a volume group the driver did
// not create, or the volume group holds logical volumes it did not create.
var ErrInUse = errors.New("disk is in use")

// Names returns the volume group and logical volume names for a volume.
func Names(wwn string) (vg, lv string, err error) {
	if len(wwn) < 16 || !hexRe.MatchString(wwn) {
		return "", "", fmt.Errorf("invalid WWN %q", wwn)
	}
	sum := sha256.Sum256([]byte(strings.ToUpper(wwn)))
	suffix := hex.EncodeToString(sum[:])[:nameSuffixLen]
	return vgPrefix + suffix, lvPrefix + suffix, nil
}

// IsOwnedVG reports whether a volume group name is one the driver creates.
func IsOwnedVG(vg string) bool { return ownedVGRe.MatchString(vg) }

// ValidFSType reports whether fsType is one the driver makes on AIX.
func ValidFSType(fsType string) bool {
	return fsType == FSTypeJFS || fsType == FSTypeJFS2
}

// Runner runs a command and returns its combined output.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// ExecRunner runs commands with os/exec.
type ExecRunner struct{}

// Run implements Runner.
func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	// Commands such as mkvg and logform read confirmations from stdin; give
	// them nothing rather than inheriting the plugin's stdin.
	cmd.Stdin = nil
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if err != nil {
		return out.Bytes(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(out.String()))
	}
	return out.Bytes(), nil
}

// Manager prepares, mounts, grows and releases volumes.
type Manager struct {
	Runner Runner
	// ReadHead reads the first n bytes of a device.
	ReadHead func(path string, n int) ([]byte, error)

	// mu serialises LVM changes: AIX LVM and ODM updates take global locks,
	// and concurrent mkvg/importvg/exportvg calls fail on them.
	mu sync.Mutex
}

// New returns a Manager that runs the AIX commands.
func New() *Manager {
	return &Manager{Runner: ExecRunner{}, ReadHead: readHead}
}

func readHead(path string, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, n)
	got, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	return buf[:got], nil
}

func (m *Manager) run(ctx context.Context, name string, args ...string) (string, error) {
	klog.V(4).Infof("aixlvm: %s %s", name, strings.Join(args, " "))
	out, err := m.Runner.Run(ctx, name, args...)
	return string(out), err
}

// Volume is a prepared file system.
type Volume struct {
	VG     string
	LV     string
	FSType string
}

// Device is the logical volume's block device, which the mount table shows.
func (v *Volume) Device() string { return "/dev/" + v.LV }

// Prepare makes the file system on disk ready to mount at mountPoint:
//
//   - disk already in this volume's volume group: vary it on if needed;
//   - disk carries a VGDA but is not imported here: import the volume group
//     under this volume's name and turn off automatic varyon;
//   - disk blank: create a scalable volume group (no automatic varyon), a
//     logical volume over all of it and the file system (JFS2 with an inline
//     log; JFS with a log logical volume that crfs creates).
//
// It then makes sure /etc/filesystems has a stanza mounting the logical
// volume at mountPoint, since chfs (used to grow it) and mount by mount point
// work from that stanza.
func (m *Manager) Prepare(ctx context.Context, disk, wwn, fsType, mountPoint string) (*Volume, error) {
	if !diskRe.MatchString(disk) {
		return nil, fmt.Errorf("invalid disk name %q", disk)
	}
	if !ValidFSType(fsType) {
		return nil, fmt.Errorf("unsupported file system type %q on AIX (want %s or %s)", fsType, FSTypeJFS2, FSTypeJFS)
	}
	vg, lv, err := Names(wwn)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	current, err := m.vgOfDisk(ctx, disk)
	if err != nil {
		return nil, err
	}
	imported := false
	switch {
	case current == vg:
		if err := m.varyOn(ctx, vg); err != nil {
			return nil, err
		}
	case current != NoVG:
		return nil, fmt.Errorf("%w: %s belongs to volume group %s, expected %s", ErrInUse, disk, current, vg)
	case m.hasVGDA(ctx, disk):
		klog.Infof("aixlvm: importing the volume group on %s as %s", disk, vg)
		if _, err := m.run(ctx, "importvg", "-y", vg, disk); err != nil {
			return nil, err
		}
		if _, err := m.run(ctx, "chvg", "-a", "n", vg); err != nil {
			return nil, err
		}
		imported = true
	default:
		head, err := m.ReadHead("/dev/r"+disk, BlankCheckBytes)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", disk, err)
		}
		if !IsBlank(head) {
			return nil, fmt.Errorf("%w: %s has no AIX volume group but its first %d KiB are not empty; "+
				"not formatting it (clear it first if its contents are not needed)", ErrNotBlank, disk, BlankCheckBytes/1024)
		}
		klog.Infof("aixlvm: creating volume group %s on %s", vg, disk)
		// -S scalable (grows with chvg -g), -n no varyon at boot.
		if _, err := m.run(ctx, "mkvg", "-S", "-n", "-y", vg, disk); err != nil {
			return nil, err
		}
	}

	lvs, err := m.lvs(ctx, vg)
	if err != nil {
		return nil, err
	}
	var ours *LV
	var others []string
	for i := range lvs {
		switch {
		case lvs[i].Name == lv:
			ours = &lvs[i]
		case !lvs[i].IsLog():
			others = append(others, lvs[i].Name)
		}
	}
	switch {
	case ours != nil:
		if ours.Type != fsType {
			return nil, fmt.Errorf("volume holds a %s file system, not %s", ours.Type, fsType)
		}
	case len(others) > 0 || imported:
		// Logical volumes the driver did not make, or an imported volume
		// group without this volume's: someone else's data.
		return nil, fmt.Errorf("%w: volume group %s on %s has logical volumes %v but not %s; not formatting it",
			ErrInUse, vg, disk, others, lv)
	default:
		if err := m.makeFS(ctx, vg, lv, fsType, mountPoint); err != nil {
			return nil, err
		}
	}
	if err := m.ensureStanza(ctx, lv, mountPoint); err != nil {
		return nil, err
	}
	return &Volume{VG: vg, LV: lv, FSType: fsType}, nil
}

func (m *Manager) makeFS(ctx context.Context, vg, lv, fsType, mountPoint string) error {
	out, err := m.run(ctx, "lsvg", vg)
	if err != nil {
		return err
	}
	info, err := ParseLsvg(out)
	if err != nil {
		return err
	}
	pps := info.FreePPs
	if fsType == FSTypeJFS {
		pps-- // leave a partition for the jfslog crfs creates
	}
	if pps < 1 {
		return fmt.Errorf("volume group %s has no room for a file system (%d free partitions)", vg, info.FreePPs)
	}
	klog.Infof("aixlvm: creating %s on %s (%d x %d MB)", fsType, lv, pps, info.PPSizeMB)
	if _, err := m.run(ctx, "mklv", "-y", lv, "-t", fsType, vg, fmt.Sprint(pps)); err != nil {
		return err
	}
	args := []string{"-v", fsType, "-d", lv, "-m", mountPoint, "-A", "no", "-p", "rw"}
	if fsType == FSTypeJFS2 {
		args = append(args, "-a", "logname=INLINE")
	}
	if _, err := m.run(ctx, "crfs", args...); err != nil {
		// Without this, the next attempt would find the logical volume and
		// take it for a file system.
		if _, rmErr := m.run(ctx, "rmlv", "-f", lv); rmErr != nil {
			klog.Warningf("aixlvm: removing %s after crfs failed: %v", lv, rmErr)
		}
		return err
	}
	return nil
}

// ensureStanza makes /etc/filesystems mount /dev/<lv> at mountPoint. crfs
// writes the stanza; importvg recreates it from the logical volume label,
// which is the mount point the volume was created with. The kubelet's
// staging path is the same on every node, but in case it is not, a stanza
// with another mount point is moved and a missing one is imported.
func (m *Manager) ensureStanza(ctx context.Context, lv, mountPoint string) error {
	out, err := m.run(ctx, "lsfs", "-c")
	if err != nil {
		return err
	}
	dev := "/dev/" + lv
	for _, s := range ParseLsfsC(out) {
		if s.Device != dev {
			continue
		}
		if s.MountPoint == mountPoint {
			return nil
		}
		_, err := m.run(ctx, "chfs", "-m", mountPoint, s.MountPoint)
		return err
	}
	if _, err := m.run(ctx, "chlv", "-L", mountPoint, lv); err != nil {
		return err
	}
	_, err = m.run(ctx, "imfs", "-l", lv)
	return err
}

// Mount mounts the file system whose stanza names mountPoint, adding
// options to the stanza's (which carry the log= option).
func (m *Manager) Mount(ctx context.Context, mountPoint string, options []string) error {
	args := []string{}
	if len(options) > 0 {
		args = append(args, "-o", strings.Join(options, ","))
	}
	_, err := m.run(ctx, "mount", append(args, mountPoint)...)
	return err
}

// Release varies off and exports the volume group on disk, so this node
// forgets it while the disk keeps it. A disk in no volume group is left
// alone; one in a volume group the driver did not create is refused.
func (m *Manager) Release(ctx context.Context, disk string) error {
	if !diskRe.MatchString(disk) {
		return fmt.Errorf("invalid disk name %q", disk)
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	vg, err := m.vgOfDisk(ctx, disk)
	if err != nil {
		return err
	}
	if vg == NoVG {
		return nil
	}
	if !IsOwnedVG(vg) {
		return fmt.Errorf("%w: %s belongs to volume group %s, which this driver did not create", ErrInUse, disk, vg)
	}
	active, err := m.activeVGs(ctx)
	if err != nil {
		return err
	}
	if active[vg] {
		lvs, err := m.lvs(ctx, vg)
		if err != nil {
			return err
		}
		for _, l := range lvs {
			if l.Open() && !l.IsLog() {
				return fmt.Errorf("logical volume %s in %s is still open (mounted at %s)", l.Name, vg, l.MountPoint)
			}
		}
		if _, err := m.run(ctx, "varyoffvg", vg); err != nil {
			return err
		}
	}
	klog.Infof("aixlvm: exporting volume group %s from %s", vg, disk)
	_, err = m.run(ctx, "exportvg", vg)
	return err
}

// DiskOfLV returns the disk holding a logical volume.
func (m *Manager) DiskOfLV(ctx context.Context, lv string) (string, error) {
	vg, err := m.vgOfLV(ctx, lv)
	if err != nil {
		return "", err
	}
	out, err := m.run(ctx, "lsvg", "-p", vg)
	if err != nil {
		return "", err
	}
	disks := ParseLsvgP(out)
	if len(disks) != 1 {
		return "", fmt.Errorf("volume group %s spans %d disks, expected one", vg, len(disks))
	}
	return disks[0], nil
}

// Grow takes in space the disk has gained: chvg -g picks up the new disk
// size, then chfs grows the file system mounted at mountPoint (and its
// logical volume) by the volume group's free space.
func (m *Manager) Grow(ctx context.Context, lv, mountPoint string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	vg, err := m.vgOfLV(ctx, lv)
	if err != nil {
		return err
	}
	if !IsOwnedVG(vg) {
		return fmt.Errorf("%w: %s is in volume group %s, which this driver did not create", ErrInUse, lv, vg)
	}
	if _, err := m.run(ctx, "chvg", "-g", vg); err != nil {
		return err
	}
	out, err := m.run(ctx, "lsvg", vg)
	if err != nil {
		return err
	}
	info, err := ParseLsvg(out)
	if err != nil {
		return err
	}
	if info.FreePPs == 0 {
		klog.V(4).Infof("aixlvm: %s has no free space to grow into", vg)
		return nil
	}
	grow := info.FreePPs * info.PPSizeMB
	klog.Infof("aixlvm: growing %s by %d MB", mountPoint, grow)
	_, err = m.run(ctx, "chfs", "-a", fmt.Sprintf("size=+%dM", grow), mountPoint)
	return err
}

func (m *Manager) vgOfDisk(ctx context.Context, disk string) (string, error) {
	out, err := m.run(ctx, "lspv")
	if err != nil {
		return "", err
	}
	for _, pv := range ParseLspv(out) {
		if pv.Name == disk {
			return pv.VG, nil
		}
	}
	return "", fmt.Errorf("%s not listed by lspv", disk)
}

func (m *Manager) vgOfLV(ctx context.Context, lv string) (string, error) {
	if !lvNameRe.MatchString(lv) {
		return "", fmt.Errorf("invalid logical volume name %q", lv)
	}
	out, err := m.run(ctx, "lslv", lv)
	if err != nil {
		return "", err
	}
	return ParseLslvVG(out)
}

// hasVGDA reports whether the disk carries a volume group descriptor area.
// lqueryvg exits non-zero when it finds none.
func (m *Manager) hasVGDA(ctx context.Context, disk string) bool {
	_, err := m.run(ctx, "lqueryvg", "-p", disk, "-v")
	return err == nil
}

func (m *Manager) activeVGs(ctx context.Context) (map[string]bool, error) {
	out, err := m.run(ctx, "lsvg", "-o")
	if err != nil {
		return nil, err
	}
	active := map[string]bool{}
	for _, f := range strings.Fields(out) {
		active[f] = true
	}
	return active, nil
}

func (m *Manager) varyOn(ctx context.Context, vg string) error {
	active, err := m.activeVGs(ctx)
	if err != nil || active[vg] {
		return err
	}
	_, err = m.run(ctx, "varyonvg", vg)
	return err
}

func (m *Manager) lvs(ctx context.Context, vg string) ([]LV, error) {
	out, err := m.run(ctx, "lsvg", "-l", vg)
	if err != nil {
		return nil, err
	}
	return ParseLsvgL(out), nil
}

// Default is the Manager shared by the node plugin, so that every LVM change
// on the node goes through one lock.
var Default = New()
