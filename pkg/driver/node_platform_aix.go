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

package driver

import (
	"context"
	"fmt"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"golang.org/x/sys/unix"

	"k8s.io/mount-utils"
	"k8s.io/utils/exec"

	"sigs.k8s.io/ibm-powervs-block-csi-driver/pkg/aixlvm"
	"sigs.k8s.io/ibm-powervs-block-csi-driver/pkg/aixmount"
	"sigs.k8s.io/ibm-powervs-block-csi-driver/pkg/device"
)

const (
	// FSTypeJFS represents the AIX JFS filesystem type.
	FSTypeJFS = aixlvm.FSTypeJFS
	// FSTypeJFS2 represents the AIX JFS2 filesystem type.
	FSTypeJFS2 = aixlvm.FSTypeJFS2
	// default file system type to be used when it is not provided.
	defaultFsType = FSTypeJFS2
)

// validFSTypes is the set of filesystem types the driver will format or
// mount on AIX.
var validFSTypes = map[string]bool{
	FSTypeJFS:  true,
	FSTypeJFS2: true,
}

// newNodeMounter returns a mounter backed by the AIX mount commands:
// k8s.io/mount-utils has no AIX implementation. Bind mounts (publishing a
// staged volume into a pod) become namefs mounts.
func newNodeMounter() Mounter {
	return &NodeMounter{
		mount.SafeFormatAndMount{
			Interface: aixmount.New(aixmount.NFSOptionPolicy{}),
			Exec:      exec.New(),
		},
	}
}

// FormatAndMount puts the volume on disk source (/dev/hdiskN) at target. AIX
// file systems live on logical volumes, so this prepares the volume's volume
// group, logical volume and file system (creating them on a blank disk,
// importing them from one that already has them) and mounts the file system
// through its /etc/filesystems stanza, which carries its log= option. See
// pkg/aixlvm.
func (m *NodeMounter) FormatAndMount(source, target, fstype string, options []string) error {
	ctx := context.Background()
	disk := strings.TrimPrefix(source, "/dev/")
	wwn, err := device.GetDeviceWWN(source)
	if err != nil {
		return err
	}
	if _, err := aixlvm.Default.Prepare(ctx, disk, wwn, strings.ToLower(fstype), target); err != nil {
		return err
	}
	return aixlvm.Default.Mount(ctx, target, options)
}

// aixResizer grows a JFS or JFS2 file system after its volume has grown.
type aixResizer struct {
	mounter Mounter
}

func newResizer(m Mounter) resizer {
	return &aixResizer{mounter: m}
}

// Resize finds the logical volume and file system mount behind devicePath,
// which is what the mount table shows at the volume path: the logical
// volume, or for a pod's namefs mount the staging directory it grafts.
func (r *aixResizer) Resize(devicePath, deviceMountPath string) (bool, error) {
	mps, err := r.mounter.List()
	if err != nil {
		return false, err
	}
	lvDevice, fsMount, err := resolveFSMount(mps, devicePath)
	if err != nil {
		return false, err
	}
	if err := aixlvm.Default.Grow(context.Background(), strings.TrimPrefix(lvDevice, "/dev/"), fsMount); err != nil {
		return false, err
	}
	return true, nil
}

// checkNodeVolumeCapability refuses what the AIX node cannot serve; see
// aixCheckVolumeCapability.
func checkNodeVolumeCapability(volCap *csi.VolumeCapability) error {
	return aixCheckVolumeCapability(volCap)
}

// fsInfo calls statfs directly: k8s.io/kubernetes/pkg/volume/util/fs has no
// AIX implementation.
func fsInfo(path string) (int64, int64, int64, int64, int64, int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, 0, 0, 0, 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	return statfsInfo(int64(st.Bsize), int64(st.Fsize), int64(st.Blocks), int64(st.Bfree), int64(st.Bavail),
		int64(st.Files), int64(st.Ffree))
}
