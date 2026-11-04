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

// Package aixmount implements k8s.io/mount-utils' Interface for IBM AIX.
//
// k8s.io/mount-utils has no AIX support: on aix it builds its "unsupported"
// Mounter, whose Mount, List and IsLikelyNotMountPoint all return errors.
// This package fills that gap with the native AIX commands, so it can serve
// the NFS CSI node plugin now and the kubelet's own mount-utils later.
//
// What it maps:
//
//   - Mount runs /usr/sbin/mount. NFS sources ("host:/path") are mounted with
//     the vfs chosen by the vers= option; Linux NFS options are translated
//     (see TranslateNFSOptions). A Linux bind mount ("bind" option) becomes an
//     AIX namefs mount, which grafts one directory onto another.
//   - Unmount runs /usr/sbin/umount; UnmountWithForce falls back to umount -f.
//   - List parses the output of /usr/sbin/mount, the only mount table AIX
//     exposes without cgo (there is no /proc/mounts or /etc/mtab; mntctl(2)
//     is not wrapped by x/sys/unix).
//   - IsLikelyNotMountPoint compares device numbers like the Linux mounter.
//     namefs mounts share their source's device number, so IsMountPoint
//     consults the mount table instead.
//
// AIX has no mount namespaces or mount propagation, so every mount made here
// is visible host-wide, which is what a CSI node plugin running in the global
// environment needs.
//
// The parsing and option translation are platform-independent so they are
// unit-tested on Linux; only the command execution is aix-only.
//
// This copy is taken unchanged from github.com/gilbahat/csi-driver-nfs
// (pkg/aixmount at 68a96e0) and should stay in step with it until the package
// moves to a module both drivers can import. The NFS option translation is
// unused here.
package aixmount
