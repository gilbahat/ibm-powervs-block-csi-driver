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

package aixmount

import (
	"reflect"
	"testing"

	mount "k8s.io/mount-utils"
)

// sampleMountOutput is shaped like AIX 7.3 `mount` output: blank node column
// for local file systems, remote host in the node column for NFS, a
// three-word date, and an optional options column.
const sampleMountOutput = `  node       mounted        mounted over    vfs       date        options
-------- ---------------  ---------------  ------ ------------ ---------------
         /dev/hd4         /                jfs2   Oct 30 22:19 rw,log=/dev/hd8
         /dev/hd2         /usr             jfs2   Oct 30 22:19 rw,log=/dev/hd8
         /proc            /proc            procfs Oct 30 22:19 rw
         /dev/fslv00      /var/lib/kubelet jfs2   Oct 30 22:20
nfssrv   /export/data/pvc-1 /var/lib/kubelet/pods/u1/volumes/kubernetes.io~csi/pv-1/mount nfs3   Nov 04 14:00 rw,hard,intr,vers=3
filer01.example.com /vol/shared /var/lib/kubelet/pods/u2/volumes/kubernetes.io~csi/pv-2/mount nfs4 Nov 04 14:01 rw,vers=4,hard
         /var/lib/kubelet/pods/u1/volumes/kubernetes.io~csi/pv-1/mount /wpars/c1/data namefs Nov 04 14:02 rw
`

func TestParseMountTable(t *testing.T) {
	got := ParseMountTable(sampleMountOutput)
	want := []mount.MountPoint{
		{Device: "/dev/hd4", Path: "/", Type: "jfs2", Opts: []string{"rw", "log=/dev/hd8"}},
		{Device: "/dev/hd2", Path: "/usr", Type: "jfs2", Opts: []string{"rw", "log=/dev/hd8"}},
		{Device: "/proc", Path: "/proc", Type: "procfs", Opts: []string{"rw"}},
		{Device: "/dev/fslv00", Path: "/var/lib/kubelet", Type: "jfs2"},
		{Device: "nfssrv:/export/data/pvc-1", Path: "/var/lib/kubelet/pods/u1/volumes/kubernetes.io~csi/pv-1/mount", Type: "nfs3", Opts: []string{"rw", "hard", "intr", "vers=3"}},
		{Device: "filer01.example.com:/vol/shared", Path: "/var/lib/kubelet/pods/u2/volumes/kubernetes.io~csi/pv-2/mount", Type: "nfs4", Opts: []string{"rw", "vers=4", "hard"}},
		{Device: "/var/lib/kubelet/pods/u1/volumes/kubernetes.io~csi/pv-1/mount", Path: "/wpars/c1/data", Type: "namefs", Opts: []string{"rw"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseMountTable mismatch:\n got  %+v\n want %+v", got, want)
	}
}

func TestParseMountTableIgnoresJunk(t *testing.T) {
	if got := ParseMountTable(""); len(got) != 0 {
		t.Errorf("empty output gave %v", got)
	}
	// No header separator: nothing is trusted.
	if got := ParseMountTable("         /dev/hd4  /  jfs2  Oct 30 22:19 rw\n"); len(got) != 0 {
		t.Errorf("output without header gave %v", got)
	}
	// Short rows are skipped.
	out := "---\n         /dev/hd4 /\nnfssrv /x /y nfs3\n"
	if got := ParseMountTable(out); len(got) != 0 {
		t.Errorf("short rows gave %v", got)
	}
}

func TestMountRefs(t *testing.T) {
	mps := ParseMountTable(sampleMountOutput)
	pv1 := "/var/lib/kubelet/pods/u1/volumes/kubernetes.io~csi/pv-1/mount"
	if got, want := mountRefs(mps, pv1), []string{"/wpars/c1/data"}; !reflect.DeepEqual(got, want) {
		t.Errorf("mountRefs(pv-1) = %v, want %v", got, want)
	}
	pv2 := "/var/lib/kubelet/pods/u2/volumes/kubernetes.io~csi/pv-2/mount"
	if got := mountRefs(mps, pv2); len(got) != 0 {
		t.Errorf("mountRefs(pv-2) = %v, want none", got)
	}
	// Two pods mounting the same share are references to each other.
	extra := append(mps, mount.MountPoint{Device: "nfssrv:/export/data/pvc-1", Path: "/other", Type: "nfs3"})
	if got, want := mountRefs(extra, pv1), []string{"/wpars/c1/data", "/other"}; !reflect.DeepEqual(got, want) {
		t.Errorf("mountRefs(pv-1, shared) = %v, want %v", got, want)
	}
}
