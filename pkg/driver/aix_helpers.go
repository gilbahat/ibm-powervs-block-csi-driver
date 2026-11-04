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

// Helpers for the AIX node service, kept free of build tags so they are
// unit-tested on Linux.

package driver

import (
	"fmt"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"k8s.io/mount-utils"
)

// resolveFSMount follows a device path from the mount table to the JFS or
// JFS2 mount behind it. devicePath is either the logical volume (/dev/<lv>)
// or, for a pod's volume, the directory that a namefs mount grafts, which is
// itself a mount point further down the chain.
func resolveFSMount(mps []mount.MountPoint, devicePath string) (lvDevice, mountPoint string, err error) {
	cur := devicePath
	for hops := 0; hops < 8; hops++ {
		if strings.HasPrefix(cur, "/dev/") {
			for _, mp := range mps {
				if mp.Device == cur && (mp.Type == "jfs2" || mp.Type == "jfs") {
					return cur, mp.Path, nil
				}
			}
			return "", "", fmt.Errorf("%s is not mounted as a JFS or JFS2 file system", cur)
		}
		next := ""
		for _, mp := range mps {
			if mp.Path == cur {
				next = mp.Device
				break
			}
		}
		if next == "" {
			return "", "", fmt.Errorf("%s is not a mount point", cur)
		}
		cur = next
	}
	return "", "", fmt.Errorf("mount chain from %s is too long", devicePath)
}

// statfsInfo turns AIX statfs counts into what FSInfo returns: available,
// capacity and used bytes, then total, free and used inodes. Block counts
// are taken to be in units of the fundamental block size f_fsize, as
// statvfs counts are in f_frsize; f_bsize is used if f_fsize is zero. For
// JFS2 the two are both 4096; they differ for JFS with fragments under 4 KiB.
func statfsInfo(bsize, fsize, blocks, bfree, bavail, files, ffree int64) (int64, int64, int64, int64, int64, int64, error) {
	unit := fsize
	if unit <= 0 {
		unit = bsize
	}
	capacity := blocks * unit
	available := bavail * unit
	used := (blocks - bfree) * unit
	return available, capacity, used, files, ffree, files - ffree, nil
}

// aixCheckVolumeCapability refuses what the AIX node cannot serve:
//
//   - raw block volumes: the AIX kubelet has no block volume path handler,
//     and AIX has no bind mount to publish a device node with;
//   - multi-node access to a mounted volume: JFS and JFS2 are not cluster
//     file systems, and varying on the volume group on a second node writes
//     to its VGDA even for a read-only mount.
func aixCheckVolumeCapability(volCap *csi.VolumeCapability) error {
	if volCap.GetBlock() != nil {
		return status.Error(codes.InvalidArgument, "raw block volumes are not supported on AIX nodes")
	}
	switch volCap.GetAccessMode().GetMode() {
	case csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
		csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER,
		csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER:
		return status.Errorf(codes.InvalidArgument, "access mode %s is not supported on AIX nodes: JFS and JFS2 are single-node file systems",
			volCap.GetAccessMode().GetMode())
	}
	return nil
}
