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
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// NoVG is how lspv shows a disk that belongs to no imported volume group.
const NoVG = "None"

// PV is one row of lspv.
type PV struct {
	Name string
	PVID string
	VG   string
}

// ParseLspv parses `lspv`:
//
//	hdisk0          00c8b12f7a1c2d3e                    rootvg          active
//	hdisk1          none                                None
func ParseLspv(out string) []PV {
	var pvs []PV
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		pvs = append(pvs, PV{Name: f[0], PVID: f[1], VG: f[2]})
	}
	return pvs
}

// VGInfo is what is needed from `lsvg <vg>`.
type VGInfo struct {
	PPSizeMB int64
	FreePPs  int64
}

var (
	ppSizeRe  = regexp.MustCompile(`PP SIZE:\s+(\d+)\s+megabyte`)
	freePPsRe = regexp.MustCompile(`FREE PPs:\s+(\d+)`)
	vgRe      = regexp.MustCompile(`VOLUME GROUP:\s+(\S+)`)
)

// ParseLsvg parses `lsvg <vg>`:
//
//	VOLUME GROUP:       pv8100a8b80003be         VG IDENTIFIER:  00c8b12f00004c00000001...
//	VG STATE:           active                   PP SIZE:        64 megabyte(s)
//	VG PERMISSION:      read/write               TOTAL PPs:      1599 (102336 megabytes)
//	MAX LVs:            256                      FREE PPs:       1599 (102336 megabytes)
func ParseLsvg(out string) (VGInfo, error) {
	pp := ppSizeRe.FindStringSubmatch(out)
	free := freePPsRe.FindStringSubmatch(out)
	if pp == nil || free == nil {
		return VGInfo{}, fmt.Errorf("unexpected lsvg output: %q", out)
	}
	ppSize, _ := strconv.ParseInt(pp[1], 10, 64)
	freePPs, _ := strconv.ParseInt(free[1], 10, 64)
	return VGInfo{PPSizeMB: ppSize, FreePPs: freePPs}, nil
}

// LV is one row of `lsvg -l <vg>`.
type LV struct {
	Name       string
	Type       string
	LPs        int64
	State      string
	MountPoint string
}

// Open reports whether the logical volume is in use (for a file system:
// mounted).
func (l LV) Open() bool { return strings.HasPrefix(l.State, "open") }

// IsLog reports whether the logical volume is a JFS or JFS2 log.
func (l LV) IsLog() bool { return l.Type == "jfslog" || l.Type == "jfs2log" }

// ParseLsvgL parses `lsvg -l <vg>`:
//
//	pv8100a8b80003be:
//	LV NAME             TYPE       LPs     PPs     PVs  LV STATE      MOUNT POINT
//	lv8100a8b80003be    jfs2       1599    1599    1    open/syncd    /var/lib/kubelet/...
//	loglv00             jfslog     1       1       1    closed/syncd  N/A
func ParseLsvgL(out string) []LV {
	var lvs []LV
	header := false
	for _, line := range strings.Split(out, "\n") {
		if !header {
			header = strings.HasPrefix(strings.TrimSpace(line), "LV NAME")
			continue
		}
		f := strings.Fields(line)
		if len(f) < 7 {
			continue
		}
		lps, _ := strconv.ParseInt(f[2], 10, 64)
		lvs = append(lvs, LV{Name: f[0], Type: f[1], LPs: lps, State: f[5], MountPoint: strings.Join(f[6:], " ")})
	}
	return lvs
}

// ParseLsvgP returns the disks named by `lsvg -p <vg>`:
//
//	pv8100a8b80003be:
//	PV_NAME           PV STATE          TOTAL PPs   FREE PPs    FREE DISTRIBUTION
//	hdisk4            active            1599        0           00..00..00..00..00
func ParseLsvgP(out string) []string {
	var disks []string
	header := false
	for _, line := range strings.Split(out, "\n") {
		if !header {
			header = strings.HasPrefix(strings.TrimSpace(line), "PV_NAME")
			continue
		}
		if f := strings.Fields(line); len(f) > 0 {
			disks = append(disks, f[0])
		}
	}
	return disks
}

// ParseLslvVG returns the volume group named by `lslv <lv>`:
//
//	LOGICAL VOLUME:     lv8100a8b80003be       VOLUME GROUP:   pv8100a8b80003be
func ParseLslvVG(out string) (string, error) {
	m := vgRe.FindStringSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("unexpected lslv output: %q", out)
	}
	return m[1], nil
}

// Stanza is one line of `lsfs -c`.
type Stanza struct {
	MountPoint string
	Device     string
	Vfs        string
}

// ParseLsfsC parses `lsfs -c`, whose lines are
// MountPoint:Device:Vfs:Nodename:Type:Size:Options:AutoMount:Acct, after a
// header line starting with '#'.
func ParseLsfsC(out string) []Stanza {
	var st []Stanza
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, ":")
		if len(f) < 3 {
			continue
		}
		st = append(st, Stanza{MountPoint: f[0], Device: f[1], Vfs: f[2]})
	}
	return st
}

// BlankCheckBytes is how much of a disk IsBlank looks at. It covers the
// signatures of the formats a reused volume is likely to carry: MBR and GPT,
// Linux LVM2, ext2/3/4, XFS, Btrfs (superblock at 64 KiB) and AIX VGDAs.
const BlankCheckBytes = 1024 * 1024

// aixIPLMagic is "IBMA" in EBCDIC, which starts an AIX IPL record. chdev
// -a pv=yes writes one to hold a PVID, which does not make a disk non-blank.
var aixIPLMagic = []byte{0xC9, 0xC2, 0xD4, 0xC1}

// IsBlank reports whether the start of a disk holds nothing but zeros, other
// than an AIX IPL record in the first sector.
func IsBlank(head []byte) bool {
	start := 0
	if len(head) >= 512 && string(head[:4]) == string(aixIPLMagic) {
		start = 512
	}
	for _, b := range head[start:] {
		if b != 0 {
			return false
		}
	}
	return true
}
