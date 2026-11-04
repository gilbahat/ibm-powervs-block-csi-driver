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
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

const (
	testWWN  = "60050768108100A8B8000000000003BE"
	testVG   = "pvsd9390ce68c0a"
	testLV   = "pvld9390ce68c0a"
	testDisk = "hdisk4"
	testMP   = "/var/lib/kubelet/plugins/kubernetes.io/csi/powervs.csi.ibm.com/abc/globalmount"
)

// fakeRunner answers commands from a table keyed by the full command line.
// Unlisted commands succeed with no output. It records every call.
type fakeRunner struct {
	out   map[string]string
	fail  map[string]bool
	calls []string
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, line)
	if f.fail[line] {
		return []byte("failed"), fmt.Errorf("%s: exit status 1", line)
	}
	return []byte(f.out[line]), nil
}

func newManager(f *fakeRunner, head []byte) *Manager {
	return &Manager{Runner: f, ReadHead: func(string, int) ([]byte, error) { return head, nil }}
}

func lspv(vg string) string {
	return "hdisk0          00c8b12f7a1c2d3e                    rootvg          active\n" +
		"hdisk4          none                                " + vg + "\n"
}

const lsvgOut = `VOLUME GROUP:       pvsd9390ce68c0a          VG IDENTIFIER:  00c8b12f00004c000000017a1b2c3d4e
VG STATE:           active                   PP SIZE:        64 megabyte(s)
VG PERMISSION:      read/write               TOTAL PPs:      1599 (102336 megabytes)
MAX LVs:            256                      FREE PPs:       1599 (102336 megabytes)
LVs:                0                        USED PPs:       0 (0 megabytes)
`

func lsvgL(rows ...string) string {
	return testVG + ":\nLV NAME             TYPE       LPs     PPs     PVs  LV STATE      MOUNT POINT\n" +
		strings.Join(rows, "\n") + "\n"
}

func stanzas(rows ...string) string {
	return "#MountPoint:Device:Vfs:Nodename:Type:Size:Options:AutoMount:Acct\n" +
		"/:/dev/hd4:jfs2::bootfs:2097152:rw:yes:no\n" + strings.Join(rows, "\n") + "\n"
}

func TestPrepareBlankDiskJFS2(t *testing.T) {
	f := &fakeRunner{
		out: map[string]string{
			"lspv":              lspv(NoVG),
			"lsvg " + testVG:    lsvgOut,
			"lsvg -l " + testVG: lsvgL(),
			"lsfs -c":           stanzas(testMP + ":/dev/" + testLV + ":jfs2:::209584128:rw:no:no"),
		},
		fail: map[string]bool{"lqueryvg -p hdisk4 -v": true},
	}
	vol, err := newManager(f, make([]byte, BlankCheckBytes)).Prepare(context.Background(), testDisk, testWWN, FSTypeJFS2, testMP)
	if err != nil {
		t.Fatal(err)
	}
	if vol.Device() != "/dev/"+testLV || vol.VG != testVG {
		t.Errorf("got %+v", vol)
	}
	want := []string{
		"lspv",
		"lqueryvg -p hdisk4 -v",
		"mkvg -S -n -y " + testVG + " hdisk4",
		"lsvg -l " + testVG,
		"lsvg " + testVG,
		"mklv -y " + testLV + " -t jfs2 " + testVG + " 1599",
		"crfs -v jfs2 -d " + testLV + " -m " + testMP + " -A no -p rw -a logname=INLINE",
		"lsfs -c",
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
}

func TestPrepareBlankDiskJFSLeavesRoomForLog(t *testing.T) {
	f := &fakeRunner{
		out: map[string]string{
			"lspv":              lspv(NoVG),
			"lsvg " + testVG:    lsvgOut,
			"lsvg -l " + testVG: lsvgL(),
			"lsfs -c":           stanzas(testMP + ":/dev/" + testLV + ":jfs:::0:rw:no:no"),
		},
		fail: map[string]bool{"lqueryvg -p hdisk4 -v": true},
	}
	if _, err := newManager(f, make([]byte, BlankCheckBytes)).Prepare(context.Background(), testDisk, testWWN, FSTypeJFS, testMP); err != nil {
		t.Fatal(err)
	}
	mustCall(t, f, "mklv -y "+testLV+" -t jfs "+testVG+" 1598")
	mustCall(t, f, "crfs -v jfs -d "+testLV+" -m "+testMP+" -A no -p rw")
}

func TestPrepareRefusesNonBlankDisk(t *testing.T) {
	f := &fakeRunner{out: map[string]string{"lspv": lspv(NoVG)}, fail: map[string]bool{"lqueryvg -p hdisk4 -v": true}}
	head := make([]byte, BlankCheckBytes)
	copy(head[1024+56:], []byte{0x53, 0xEF}) // ext4 superblock magic
	_, err := newManager(f, head).Prepare(context.Background(), testDisk, testWWN, FSTypeJFS2, testMP)
	if !errors.Is(err, ErrNotBlank) {
		t.Fatalf("got %v, want ErrNotBlank", err)
	}
	mustNotCall(t, f, "mkvg")
}

func TestPrepareImportsExistingVolume(t *testing.T) {
	f := &fakeRunner{
		out: map[string]string{
			"lspv":              lspv(NoVG),
			"lsvg -l " + testVG: lsvgL(testLV + "    jfs2       1599    1599    1    closed/syncd  /old/mount"),
			"lsfs -c":           stanzas(),
		},
	}
	if _, err := newManager(f, nil).Prepare(context.Background(), testDisk, testWWN, FSTypeJFS2, testMP); err != nil {
		t.Fatal(err)
	}
	mustCall(t, f, "importvg -y "+testVG+" hdisk4")
	mustCall(t, f, "chvg -a n "+testVG)
	mustCall(t, f, "chlv -L "+testMP+" "+testLV)
	mustCall(t, f, "imfs -l "+testLV)
	mustNotCall(t, f, "mkvg")
	mustNotCall(t, f, "crfs")
}

func TestPrepareMovesStanzaToNewMountPoint(t *testing.T) {
	f := &fakeRunner{
		out: map[string]string{
			"lspv":              lspv(testVG),
			"lsvg -o":           "rootvg\n" + testVG + "\n",
			"lsvg -l " + testVG: lsvgL(testLV + "    jfs2       1599    1599    1    closed/syncd  /old/mount"),
			"lsfs -c":           stanzas("/old/mount:/dev/" + testLV + ":jfs2:::0:rw:no:no"),
		},
	}
	if _, err := newManager(f, nil).Prepare(context.Background(), testDisk, testWWN, FSTypeJFS2, testMP); err != nil {
		t.Fatal(err)
	}
	mustCall(t, f, "chfs -m "+testMP+" /old/mount")
	mustNotCall(t, f, "varyonvg")
}

func TestPrepareVariesOnOwnInactiveVG(t *testing.T) {
	f := &fakeRunner{
		out: map[string]string{
			"lspv":              lspv(testVG),
			"lsvg -o":           "rootvg\n",
			"lsvg -l " + testVG: lsvgL(testLV + "    jfs2       1599    1599    1    closed/syncd  " + testMP),
			"lsfs -c":           stanzas(testMP + ":/dev/" + testLV + ":jfs2:::0:rw:no:no"),
		},
	}
	if _, err := newManager(f, nil).Prepare(context.Background(), testDisk, testWWN, FSTypeJFS2, testMP); err != nil {
		t.Fatal(err)
	}
	mustCall(t, f, "varyonvg "+testVG)
}

func TestPrepareRefusesOtherVolumeGroups(t *testing.T) {
	f := &fakeRunner{out: map[string]string{"lspv": lspv("datavg")}}
	_, err := newManager(f, nil).Prepare(context.Background(), testDisk, testWWN, FSTypeJFS2, testMP)
	if !errors.Is(err, ErrInUse) {
		t.Fatalf("got %v, want ErrInUse", err)
	}
}

func TestPrepareRefusesImportedVGWithoutOurLV(t *testing.T) {
	f := &fakeRunner{
		out: map[string]string{
			"lspv":              lspv(NoVG),
			"lsvg -l " + testVG: lsvgL("fslv00              jfs2       10      10      1    closed/syncd  /data"),
		},
	}
	_, err := newManager(f, nil).Prepare(context.Background(), testDisk, testWWN, FSTypeJFS2, testMP)
	if !errors.Is(err, ErrInUse) {
		t.Fatalf("got %v, want ErrInUse", err)
	}
	mustNotCall(t, f, "mklv")
}

func TestPrepareRefusesFSTypeMismatch(t *testing.T) {
	f := &fakeRunner{
		out: map[string]string{
			"lspv":              lspv(testVG),
			"lsvg -o":           testVG,
			"lsvg -l " + testVG: lsvgL(testLV + "    jfs        1598    1598    1    closed/syncd  " + testMP),
		},
	}
	_, err := newManager(f, nil).Prepare(context.Background(), testDisk, testWWN, FSTypeJFS2, testMP)
	if err == nil || !strings.Contains(err.Error(), "holds a jfs file system") {
		t.Fatalf("got %v", err)
	}
}

func TestPrepareRetriesAfterJFSLogLeftBehind(t *testing.T) {
	// A crfs that failed after creating the jfslog leaves only the log.
	f := &fakeRunner{
		out: map[string]string{
			"lspv":              lspv(testVG),
			"lsvg -o":           testVG,
			"lsvg " + testVG:    lsvgOut,
			"lsvg -l " + testVG: lsvgL("loglv00             jfslog     1       1       1    closed/syncd  N/A"),
			"lsfs -c":           stanzas(testMP + ":/dev/" + testLV + ":jfs:::0:rw:no:no"),
		},
	}
	if _, err := newManager(f, nil).Prepare(context.Background(), testDisk, testWWN, FSTypeJFS, testMP); err != nil {
		t.Fatal(err)
	}
	mustCall(t, f, "mklv -y "+testLV+" -t jfs "+testVG+" 1598")
}

func TestPrepareRemovesLVWhenCrfsFails(t *testing.T) {
	crfs := "crfs -v jfs2 -d " + testLV + " -m " + testMP + " -A no -p rw -a logname=INLINE"
	f := &fakeRunner{
		out: map[string]string{
			"lspv":              lspv(NoVG),
			"lsvg " + testVG:    lsvgOut,
			"lsvg -l " + testVG: lsvgL(),
		},
		fail: map[string]bool{"lqueryvg -p hdisk4 -v": true, crfs: true},
	}
	if _, err := newManager(f, make([]byte, BlankCheckBytes)).Prepare(context.Background(), testDisk, testWWN, FSTypeJFS2, testMP); err == nil {
		t.Fatal("expected an error")
	}
	mustCall(t, f, "rmlv -f "+testLV)
}

func TestPrepareRejectsBadInput(t *testing.T) {
	m := newManager(&fakeRunner{}, nil)
	for _, c := range []struct{ disk, wwn, fs string }{
		{"hdisk4; rm -rf /", testWWN, FSTypeJFS2},
		{testDisk, "nothex-nothex-nothex", FSTypeJFS2},
		{testDisk, testWWN, "ext4"},
	} {
		if _, err := m.Prepare(context.Background(), c.disk, c.wwn, c.fs, testMP); err == nil {
			t.Errorf("Prepare(%q, %q, %q) succeeded", c.disk, c.wwn, c.fs)
		}
	}
}

func TestMount(t *testing.T) {
	f := &fakeRunner{}
	m := newManager(f, nil)
	if err := m.Mount(context.Background(), testMP, []string{"rbrw", "noatime"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Mount(context.Background(), testMP, nil); err != nil {
		t.Fatal(err)
	}
	want := []string{"mount -o rbrw,noatime " + testMP, "mount " + testMP}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("calls %v, want %v", f.calls, want)
	}
}

func TestRelease(t *testing.T) {
	f := &fakeRunner{
		out: map[string]string{
			"lspv":    lspv(testVG),
			"lsvg -o": "rootvg\n" + testVG + "\n",
			"lsvg -l " + testVG: lsvgL(testLV+"    jfs        1598    1598    1    closed/syncd  "+testMP,
				"loglv00             jfslog     1       1       1    open/syncd    N/A"),
		},
	}
	if err := newManager(f, nil).Release(context.Background(), testDisk); err != nil {
		t.Fatal(err)
	}
	mustCall(t, f, "varyoffvg "+testVG)
	mustCall(t, f, "exportvg "+testVG)
}

func TestReleaseInactiveVGOnlyExports(t *testing.T) {
	f := &fakeRunner{out: map[string]string{"lspv": lspv(testVG), "lsvg -o": "rootvg\n"}}
	if err := newManager(f, nil).Release(context.Background(), testDisk); err != nil {
		t.Fatal(err)
	}
	mustNotCall(t, f, "varyoffvg")
	mustCall(t, f, "exportvg "+testVG)
}

func TestReleaseRefusesMountedOrForeign(t *testing.T) {
	mounted := &fakeRunner{
		out: map[string]string{
			"lspv":              lspv(testVG),
			"lsvg -o":           testVG,
			"lsvg -l " + testVG: lsvgL(testLV + "    jfs2       1599    1599    1    open/syncd    " + testMP),
		},
	}
	if err := newManager(mounted, nil).Release(context.Background(), testDisk); err == nil {
		t.Error("released a mounted volume group")
	}
	mustNotCall(t, mounted, "varyoffvg")

	foreign := &fakeRunner{out: map[string]string{"lspv": lspv("rootvg")}}
	if err := newManager(foreign, nil).Release(context.Background(), testDisk); !errors.Is(err, ErrInUse) {
		t.Errorf("got %v, want ErrInUse", err)
	}
	mustNotCall(t, foreign, "exportvg")
}

func TestReleaseWithoutVGIsNoop(t *testing.T) {
	f := &fakeRunner{out: map[string]string{"lspv": lspv(NoVG)}}
	if err := newManager(f, nil).Release(context.Background(), testDisk); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 {
		t.Errorf("calls %v", f.calls)
	}
}

func TestGrow(t *testing.T) {
	f := &fakeRunner{
		out: map[string]string{
			"lslv " + testLV: "LOGICAL VOLUME:     " + testLV + "        VOLUME GROUP:   " + testVG + "\n",
			"lsvg " + testVG: strings.Replace(lsvgOut, "FREE PPs:       1599 (102336", "FREE PPs:       16 (1024", 1),
		},
	}
	if err := newManager(f, nil).Grow(context.Background(), testLV, testMP); err != nil {
		t.Fatal(err)
	}
	want := []string{"lslv " + testLV, "chvg -g " + testVG, "lsvg " + testVG, "chfs -a size=+1024M " + testMP}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("calls %v, want %v", f.calls, want)
	}
}

func TestGrowRefusesForeignLV(t *testing.T) {
	f := &fakeRunner{out: map[string]string{"lslv hd4": "LOGICAL VOLUME:     hd4        VOLUME GROUP:   rootvg\n"}}
	if err := newManager(f, nil).Grow(context.Background(), "hd4", "/"); !errors.Is(err, ErrInUse) {
		t.Fatalf("got %v, want ErrInUse", err)
	}
	mustNotCall(t, f, "chfs")
}

func TestDiskOfLV(t *testing.T) {
	f := &fakeRunner{
		out: map[string]string{
			"lslv " + testLV:    "LOGICAL VOLUME:     " + testLV + "        VOLUME GROUP:   " + testVG + "\n",
			"lsvg -p " + testVG: testVG + ":\nPV_NAME           PV STATE          TOTAL PPs   FREE PPs    FREE DISTRIBUTION\nhdisk4            active            1599        0           00..00..00..00..00\n",
		},
	}
	disk, err := newManager(f, nil).DiskOfLV(context.Background(), testLV)
	if err != nil || disk != testDisk {
		t.Fatalf("got %q, %v", disk, err)
	}
}

func TestNames(t *testing.T) {
	vg, lv, err := Names(testWWN)
	if err != nil || vg != testVG || lv != testLV {
		t.Fatalf("got %q %q %v", vg, lv, err)
	}
	if len(vg) > 15 || len(lv) > 15 {
		t.Errorf("names longer than AIX allows: %q %q", vg, lv)
	}
	if !IsOwnedVG(vg) || IsOwnedVG("rootvg") || IsOwnedVG("pvsdata") {
		t.Error("IsOwnedVG")
	}
}

func TestIsBlank(t *testing.T) {
	zero := make([]byte, BlankCheckBytes)
	if !IsBlank(zero) {
		t.Error("zeros not blank")
	}
	withPVID := make([]byte, BlankCheckBytes)
	copy(withPVID, aixIPLMagic)
	copy(withPVID[0x80:], []byte{0x00, 0xc8, 0xb1, 0x2f, 0x7a, 0x1c, 0x2d, 0x3e})
	if !IsBlank(withPVID) {
		t.Error("IPL record with a PVID not blank")
	}
	xfs := make([]byte, BlankCheckBytes)
	copy(xfs, "XFSB")
	if IsBlank(xfs) {
		t.Error("XFS superblock blank")
	}
	lvmRecord := append([]byte{}, withPVID...)
	copy(lvmRecord[7*512:], "_LVM")
	if IsBlank(lvmRecord) {
		t.Error("AIX LVM record blank")
	}
}

func mustCall(t *testing.T, f *fakeRunner, line string) {
	t.Helper()
	for _, c := range f.calls {
		if c == line {
			return
		}
	}
	t.Errorf("missing call %q in:\n%s", line, strings.Join(f.calls, "\n"))
}

func mustNotCall(t *testing.T, f *fakeRunner, prefix string) {
	t.Helper()
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			t.Errorf("unexpected call %q", c)
		}
	}
}

func TestNamesDifferForSameSequenceOnOtherControllers(t *testing.T) {
	a, _, _ := Names("600507681081818C2000000000004F48")
	b, _, _ := Names("600507680C8080B3D800000000004F48")
	if a == b {
		t.Errorf("volumes from two controllers with one sequence number share volume group %q", a)
	}
	lower, _, _ := Names("600507681081818c2000000000004f48")
	if lower != a {
		t.Errorf("name depends on WWN case: %q vs %q", lower, a)
	}
}
