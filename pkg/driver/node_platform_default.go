//go:build !aix

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
	"github.com/container-storage-interface/spec/lib/go/csi"

	"k8s.io/kubernetes/pkg/volume/util/fs"
	"k8s.io/mount-utils"
	"k8s.io/utils/exec"
)

const (
	// default file system type to be used when it is not provided.
	defaultFsType = FSTypeExt4
)

// validFSTypes is the set of filesystem types the driver will format or mount.
// Requests specifying any other type are rejected before reaching mkfs or mount.
var validFSTypes = map[string]bool{
	FSTypeExt2: true,
	FSTypeExt3: true,
	FSTypeExt4: true,
	FSTypeXfs:  true,
}

func newNodeMounter() Mounter {
	return &NodeMounter{
		mount.SafeFormatAndMount{
			Interface: mount.New(""),
			Exec:      exec.New(),
		},
	}
}

func newResizer(m Mounter) resizer {
	return mount.NewResizeFs(m.(*NodeMounter).Exec)
}

// checkNodeVolumeCapability accepts every capability isValidVolumeCapabilities
// accepts.
func checkNodeVolumeCapability(_ *csi.VolumeCapability) error {
	return nil
}

func fsInfo(path string) (int64, int64, int64, int64, int64, int64, error) {
	return fs.Info(path)
}
