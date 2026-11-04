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
	"strings"

	mount "k8s.io/mount-utils"
)

// ParseMountTable parses the output of /usr/sbin/mount run with no
// arguments, which on AIX looks like:
//
//	  node       mounted        mounted over    vfs       date        options
//	-------- ---------------  ---------------  ------ ------------ ---------------
//	         /dev/hd4         /                jfs2   Oct 30 22:19 rw,log=/dev/hd8
//	nfssrv   /export/data     /mnt/data        nfs3   Nov 04 14:00 rw,hard,intr
//
// Local mounts leave the node column blank, so their rows start with
// whitespace. Remote mounts are returned with Device set to "node:path", the
// same form a Linux mount table uses for NFS. The date is always three
// words; the options column may be empty.
func ParseMountTable(output string) []mount.MountPoint {
	var mps []mount.MountPoint
	headerDone := false
	for _, line := range strings.Split(output, "\n") {
		if !headerDone {
			if strings.HasPrefix(strings.TrimSpace(line), "---") {
				headerDone = true
			}
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}

		local := line[0] == ' ' || line[0] == '\t'
		var device, path, vfs string
		var rest []string
		if local {
			// mounted, mounted over, vfs, date (3 words)[, options]
			if len(fields) < 6 {
				continue
			}
			device, path, vfs, rest = fields[0], fields[1], fields[2], fields[6:]
		} else {
			// node, mounted, mounted over, vfs, date (3 words)[, options]
			if len(fields) < 7 {
				continue
			}
			device = fields[0] + ":" + fields[1]
			path, vfs, rest = fields[2], fields[3], fields[7:]
		}

		mp := mount.MountPoint{Device: device, Path: path, Type: vfs}
		if len(rest) > 0 {
			mp.Opts = strings.Split(rest[0], ",")
		}
		mps = append(mps, mp)
	}
	return mps
}

// mountRefs returns the mount points, other than pathname, that show the
// same file system as pathname: those mounted from the same device, and
// namefs mounts whose source is pathname or a directory under it.
func mountRefs(mps []mount.MountPoint, pathname string) []string {
	device := ""
	for _, mp := range mps {
		if mp.Path == pathname {
			device = mp.Device
		}
	}
	refs := []string{}
	for _, mp := range mps {
		if mp.Path == pathname {
			continue
		}
		sameDevice := device != "" && mp.Device == device
		graftOfPath := mp.Type == "namefs" &&
			(mp.Device == pathname || strings.HasPrefix(mp.Device, strings.TrimSuffix(pathname, "/")+"/"))
		if sameDevice || graftOfPath {
			refs = append(refs, mp.Path)
		}
	}
	return refs
}
