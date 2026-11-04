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

	// ppSizeMB and maxPPsK size every volume group the driver makes: 64 MB
	// partitions and up to 512 x 1024 of them, so a volume can grow to 32 TB.
	// Left to itself, mkvg sizes partitions for the disk's current size, and
	// chvg -g then fails once a small volume has grown a few times over.
	ppSizeMB = 64
	maxPPsK  = 512
)

var (
	ownedVGRe = regexp.MustCompile(`^` + vgPrefix + `[0-9a-f]{` + fmt.Sprint(nameSuffixLen) + `}$`)
	hexRe     = regexp.MustCompile(`^[0-9a-fA-F]+$`)
	diskRe    = regexp.MustCompile(`^hdisk[0-9]+$`)
	lvNameRe  = regexp.MustCompile(`^[A-Za-z0-9_.]{1,15}$`)
	ownedLVRe = regexp.MustCompile(`^` + lvPrefix + `[0-9a-f]{` + fmt.Sprint(nameSuffixLen) + `}$`)
	vgidRe    = regexp.MustCompile(`[0-9a-fA-F]{16,}`)
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

// ExecRunner runs commands with os/exec. A zero Timeout means none: the LVM
// commands are not given one, since killing mkvg, importvg or crfs halfway
// can leave the ODM and the VGDA out of step.
type ExecRunner struct {
	Timeout time.Duration
}

// Run implements Runner.
func (r ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, name, args...)
	// Commands such as mkvg read confirmations from stdin; give them nothing
	// rather than inheriting the plugin's stdin.
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

// readFull reads BlankCheckBytes from the start of a device. A short read is
// an error: a blank check on less than that would pass by default.
func (m *Manager) readFull(path string) ([]byte, error) {
	head, err := m.ReadHead(path, BlankCheckBytes)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if len(head) < BlankCheckBytes {
		return nil, fmt.Errorf("reading %s: got %d bytes, want %d", path, len(head), BlankCheckBytes)
	}
	return head, nil
}

func (m *Manager) run(ctx context.Context, name string, args ...string) (string, error) {
	klog.V(4).Infof("aixlvm: %s %s", name, strings.Join(args, " "))
	out, err := m.Runner.Run(ctx, name, args...)
	return string(out), err
}

// Lock and Unlock hold the LVM lock for callers that change disks outside
// the Manager (cfgmgr, rmdev), so those never interleave with an LVM change.
func (m *Manager) Lock()   { m.mu.Lock() }
func (m *Manager) Unlock() { m.mu.Unlock() }

// Quiesce waits for any LVM change in progress and blocks further ones. The
// plugin calls it before exiting, so a stop never cuts a change short.
func (m *Manager) Quiesce() { m.mu.Lock() }

// Volume is a prepared file system.
type Volume struct {
	VG     string
	LV     string
	FSType string
	// Log is the mount's log= option: INLINE for JFS2, the jfslog device
	// for JFS.
	Log string
}

// Device is the logical volume's block device, which the mount table shows.
func (v *Volume) Device() string { return "/dev/" + v.LV }

// Prepare makes the file system on disk ready to mount at mountPoint:
//
//   - disk already in this volume's volume group: vary it on if needed;
//   - disk carries a VGDA but is not imported here: import the volume group
//     under this volume's name, check it holds a file system the driver made
//     (exporting it again if not), and turn off automatic varyon;
//   - disk blank: create a scalable volume group (no automatic varyon), a
//     logical volume over all of it and the file system (JFS2 with an inline
//     log; JFS with a log logical volume that crfs creates).
//
// A clone of a volume carries its source's logical volume name; a volume
// group holding exactly one driver-made file system is accepted and the
// logical volume renamed to this volume's.
//
// It then makes sure /etc/filesystems has a stanza mounting the logical
// volume at mountPoint, since chfs (used to grow it) works from that stanza.
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
	imported, created := false, false
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
		imported = true
	default:
		head, err := m.readFull("/dev/r" + disk)
		if err != nil {
			return nil, err
		}
		if !IsBlank(head) {
			return nil, fmt.Errorf("%w: %s has no AIX volume group but its first %d KiB are not empty; "+
				"not formatting it (clear it first if its contents are not needed)", ErrNotBlank, disk, BlankCheckBytes/1024)
		}
		klog.Infof("aixlvm: creating volume group %s on %s", vg, disk)
		// -S scalable (grows with chvg -g), -n no varyon at boot.
		if _, err := m.run(ctx, "mkvg", "-S", "-n", "-s", fmt.Sprint(ppSizeMB), "-P", fmt.Sprint(maxPPsK), "-y", vg, disk); err != nil {
			return nil, err
		}
		created = true
	}

	vol, err := m.prepareFS(ctx, disk, vg, lv, fsType, mountPoint, imported, created)
	if err != nil {
		if imported {
			// Leave a refused or broken volume group as it was found: not
			// imported on this node.
			m.unimport(ctx, vg)
		}
		return nil, err
	}
	if imported {
		if _, err := m.run(ctx, "chvg", "-a", "n", vg); err != nil {
			return nil, err
		}
	}
	return vol, nil
}

func (m *Manager) prepareFS(ctx context.Context, disk, vg, lv, fsType, mountPoint string, imported, created bool) (*Volume, error) {
	lvs, err := m.lvs(ctx, vg)
	if err != nil {
		return nil, err
	}
	var ours *LV
	var owned, others []string
	for i := range lvs {
		switch {
		case lvs[i].Name == lv:
			ours = &lvs[i]
		case lvs[i].IsLog():
		case ownedLVRe.MatchString(lvs[i].Name) && ValidFSType(lvs[i].Type):
			owned = append(owned, lvs[i].Name)
		default:
			others = append(others, lvs[i].Name)
		}
	}
	if len(others) > 0 || ours != nil && len(owned) > 0 || len(owned) > 1 {
		return nil, fmt.Errorf("%w: volume group %s on %s holds logical volumes %v the driver did not make for this volume; not using it",
			ErrInUse, vg, disk, append(others, owned...))
	}
	if ours == nil && len(owned) == 1 && !created {
		// A clone: the file system is its source's, under the source's name.
		klog.Infof("aixlvm: renaming %s to %s in %s", owned[0], lv, vg)
		// Drop the stanza importvg made for the old name first, so the one
		// ensureStanza makes for the new name is the only one for the path.
		if _, err := m.run(ctx, "imfs", "-x", "-l", owned[0]); err != nil {
			return nil, err
		}
		if _, err := m.run(ctx, "chlv", "-n", lv, owned[0]); err != nil {
			return nil, err
		}
		if lvs, err = m.lvs(ctx, vg); err != nil {
			return nil, err
		}
		for i := range lvs {
			if lvs[i].Name == lv {
				ours = &lvs[i]
			}
		}
		if ours == nil {
			return nil, fmt.Errorf("%s not found in %s after renaming", lv, vg)
		}
	}

	stanzas, err := m.stanzas(ctx)
	if err != nil {
		return nil, err
	}
	switch {
	case ours == nil && imported:
		return nil, fmt.Errorf("%w: volume group %s on %s holds no file system the driver made; not using it", ErrInUse, vg, disk)
	case ours == nil:
		if err := m.makeFS(ctx, vg, lv, fsType, mountPoint); err != nil {
			return nil, err
		}
	case ours.Type != fsType:
		return nil, fmt.Errorf("volume holds a %s file system, not %s", ours.Type, fsType)
	case !imported && !hasStanza(stanzas, "/dev/"+lv):
		// The logical volume exists but no file system was recorded for it:
		// crfs failed and so did removing the logical volume. Finish the job
		// if nothing was written to it.
		head, err := m.readFull("/dev/r" + lv)
		if err != nil {
			return nil, err
		}
		if !IsBlank(head) {
			return nil, fmt.Errorf("%s has no /etc/filesystems entry and is not blank; not formatting it", lv)
		}
		klog.Infof("aixlvm: %s has no file system yet, creating it", lv)
		if err := m.crfs(ctx, lv, fsType, mountPoint); err != nil {
			return nil, err
		}
	}

	if lvs, err = m.lvs(ctx, vg); err != nil {
		return nil, err
	}
	log := "INLINE"
	if fsType == FSTypeJFS {
		log = ""
		for _, l := range lvs {
			if l.Type == "jfslog" {
				log = "/dev/" + l.Name
				break
			}
		}
		if log == "" {
			return nil, fmt.Errorf("volume group %s has no jfslog for %s", vg, lv)
		}
	}
	if err := m.ensureStanza(ctx, lv, mountPoint); err != nil {
		return nil, err
	}
	return &Volume{VG: vg, LV: lv, FSType: fsType, Log: log}, nil
}

// unimport undoes an importvg: best effort, the error being returned is
// the one that matters.
func (m *Manager) unimport(ctx context.Context, vg string) {
	if _, err := m.run(ctx, "varyoffvg", vg); err != nil {
		klog.Warningf("aixlvm: varying off %s after a failed stage: %v", vg, err)
	}
	if _, err := m.run(ctx, "exportvg", vg); err != nil {
		klog.Warningf("aixlvm: exporting %s after a failed stage: %v", vg, err)
	}
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
	// -x: mklv otherwise caps the logical volume at 512 partitions.
	if _, err := m.run(ctx, "mklv", "-y", lv, "-t", fsType, "-x", fmt.Sprint(pps), vg, fmt.Sprint(pps)); err != nil {
		return err
	}
	if err := m.crfs(ctx, lv, fsType, mountPoint); err != nil {
		// Without this, the next attempt would find the logical volume and
		// take it for a file system.
		if _, rmErr := m.run(ctx, "rmlv", "-f", lv); rmErr != nil {
			klog.Warningf("aixlvm: removing %s after crfs failed: %v", lv, rmErr)
		}
		return err
	}
	return nil
}

func (m *Manager) crfs(ctx context.Context, lv, fsType, mountPoint string) error {
	args := []string{"-v", fsType, "-d", lv, "-m", mountPoint, "-A", "no", "-p", "rw"}
	if fsType == FSTypeJFS2 {
		args = append(args, "-a", "logname=INLINE")
	}
	_, err := m.run(ctx, "crfs", args...)
	return err
}

func (m *Manager) stanzas(ctx context.Context) ([]Stanza, error) {
	out, err := m.run(ctx, "lsfs", "-c")
	if err != nil {
		return nil, err
	}
	return ParseLsfsC(out), nil
}

func hasStanza(stanzas []Stanza, device string) bool {
	for _, s := range stanzas {
		if s.Device == device {
			return true
		}
	}
	return false
}

// ensureStanza makes /etc/filesystems mount /dev/<lv> at mountPoint. crfs
// writes the stanza; importvg recreates it from the logical volume label,
// which is the mount point the volume was created with. The kubelet's
// staging path is the same on every node, but in case it is not, a stanza
// with another mount point is moved and a missing one is imported.
func (m *Manager) ensureStanza(ctx context.Context, lv, mountPoint string) error {
	stanzas, err := m.stanzas(ctx)
	if err != nil {
		return err
	}
	dev := "/dev/" + lv
	for _, s := range stanzas {
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

// StagedLV returns the driver-made logical volume whose /etc/filesystems
// stanza mounts it at mountPoint, or "" if there is none.
func (m *Manager) StagedLV(ctx context.Context, mountPoint string) (string, error) {
	stanzas, err := m.stanzas(ctx)
	if err != nil {
		return "", err
	}
	for _, s := range stanzas {
		lv := strings.TrimPrefix(s.Device, "/dev/")
		if s.MountPoint == mountPoint && ownedLVRe.MatchString(lv) {
			return lv, nil
		}
	}
	return "", nil
}

// Mount mounts the volume's file system at mountPoint, naming the device
// and log explicitly rather than relying on how mount merges -o options
// with the stanza's.
func (m *Manager) Mount(ctx context.Context, vol *Volume, mountPoint string, options []string) error {
	opts := append([]string{"log=" + vol.Log}, options...)
	_, err := m.run(ctx, "mount", "-v", vol.FSType, "-o", strings.Join(opts, ","), vol.Device(), mountPoint)
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
	return m.release(ctx, disk)
}

func (m *Manager) release(ctx context.Context, disk string) error {
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

// ReleaseAndRemove releases the volume group on disk and removes the disk
// with rmdev -dl, under the LVM lock.
func (m *Manager) ReleaseAndRemove(ctx context.Context, disk string) error {
	if !diskRe.MatchString(disk) {
		return fmt.Errorf("invalid disk name %q", disk)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.release(ctx, disk); err != nil {
		return err
	}
	_, err := m.run(ctx, "rmdev", "-dl", disk)
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
// logical volume) by the volume group's free space. It is safe to retry:
// chvg -g failing because the disk did not change size is only logged.
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
		klog.Warningf("aixlvm: chvg -g %s: %v (continuing: the disk may already have been taken in)", vg, err)
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
	lvs, err := m.lvs(ctx, vg)
	if err != nil {
		return err
	}
	var lps int64 = -1
	for _, l := range lvs {
		if l.Name == lv {
			lps = l.LPs
		}
	}
	if lps < 0 {
		return fmt.Errorf("%s not found in %s", lv, vg)
	}
	// Raise the logical volume's partition limit so chfs can extend it.
	if _, err := m.run(ctx, "chlv", "-x", fmt.Sprint(lps+info.FreePPs), lv); err != nil {
		return err
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

// hasVGDA reports whether the disk carries a volume group descriptor area:
// lqueryvg -v prints its volume group ID, and exits non-zero when it finds
// none. An all-zero ID is taken as none too.
func (m *Manager) hasVGDA(ctx context.Context, disk string) bool {
	out, err := m.run(ctx, "lqueryvg", "-p", disk, "-v")
	if err != nil {
		return false
	}
	for _, id := range vgidRe.FindAllString(out, -1) {
		if strings.Trim(id, "0") != "" {
			return true
		}
	}
	return false
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
