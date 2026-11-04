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

package driver

import (
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"k8s.io/mount-utils"

	"sigs.k8s.io/ibm-powervs-block-csi-driver/pkg/aixmount"
)

const aixMountOutput = `  node       mounted        mounted over    vfs       date        options
-------- ---------------  ---------------  ------ ------------ ---------------
         /dev/hd4         /                jfs2   Oct 30 22:19 rw,log=/dev/hd8
         /dev/pvld9390ce68c0a /var/lib/kubelet/plugins/kubernetes.io/csi/powervs.csi.ibm.com/abc/globalmount jfs2   Nov 04 14:00 rw,log=INLINE
         /var/lib/kubelet/plugins/kubernetes.io/csi/powervs.csi.ibm.com/abc/globalmount /var/lib/kubelet/pods/uid/volumes/kubernetes.io~csi/pv1/mount namefs Nov 04 14:01 rw
`

func TestResolveFSMount(t *testing.T) {
	mps := aixmount.ParseMountTable(aixMountOutput)
	const staging = "/var/lib/kubelet/plugins/kubernetes.io/csi/powervs.csi.ibm.com/abc/globalmount"

	for _, devicePath := range []string{"/dev/pvld9390ce68c0a", staging} {
		lv, mp, err := resolveFSMount(mps, devicePath)
		if err != nil || lv != "/dev/pvld9390ce68c0a" || mp != staging {
			t.Errorf("resolveFSMount(%q) = %q, %q, %v", devicePath, lv, mp, err)
		}
	}
	if _, _, err := resolveFSMount(mps, "/not/mounted"); err == nil {
		t.Error("resolved an unmounted path")
	}
	if _, _, err := resolveFSMount(mps, "/dev/hd8"); err == nil {
		t.Error("resolved a device that is not mounted")
	}
	loop := []mount.MountPoint{{Device: "/a", Path: "/b", Type: "namefs"}, {Device: "/b", Path: "/a", Type: "namefs"}}
	if _, _, err := resolveFSMount(loop, "/a"); err == nil {
		t.Error("followed a namefs loop")
	}
}

func TestStatfsInfo(t *testing.T) {
	// 1 GiB JFS2: 262144 blocks of 4 KiB, 1000 used, 900 reserved.
	avail, capacity, used, inodes, free, iused, _ := statfsInfo(4096, 4096, 262144, 261144, 260244, 1000, 900)
	if capacity != 1<<30 || used != 1000*4096 || avail != 260244*4096 || inodes != 1000 || free != 900 || iused != 100 {
		t.Errorf("got %d %d %d %d %d %d", avail, capacity, used, inodes, free, iused)
	}
	// JFS with 512-byte fragments counts in fragments.
	_, capacity, _, _, _, _, _ = statfsInfo(4096, 512, 2048, 2048, 2048, 0, 0)
	if capacity != 1<<20 {
		t.Errorf("JFS capacity %d", capacity)
	}
	_, capacity, _, _, _, _, _ = statfsInfo(4096, 0, 256, 256, 256, 0, 0)
	if capacity != 1<<20 {
		t.Errorf("zero f_fsize capacity %d", capacity)
	}
}

func TestAIXCheckVolumeCapability(t *testing.T) {
	mountCap := func(mode csi.VolumeCapability_AccessMode_Mode) *csi.VolumeCapability {
		return &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: "jfs2"}},
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: mode},
		}
	}
	for _, mode := range []csi.VolumeCapability_AccessMode_Mode{
		csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER,
	} {
		if err := aixCheckVolumeCapability(mountCap(mode)); err != nil {
			t.Errorf("%s refused: %v", mode, err)
		}
	}
	for _, mode := range []csi.VolumeCapability_AccessMode_Mode{
		csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
		csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER,
		csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
	} {
		if status.Code(aixCheckVolumeCapability(mountCap(mode))) != codes.InvalidArgument {
			t.Errorf("%s accepted", mode)
		}
	}
	block := &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
	}
	if status.Code(aixCheckVolumeCapability(block)) != codes.InvalidArgument {
		t.Error("raw block accepted")
	}
}
