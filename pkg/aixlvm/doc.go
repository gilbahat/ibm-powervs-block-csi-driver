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

// Package aixlvm puts JFS and JFS2 file systems on PowerVS volumes attached
// to an AIX node.
//
// AIX does not put a file system directly on a disk: JFS and JFS2 live on a
// logical volume in a volume group. So each CSI volume gets its own volume
// group, holding one logical volume with the file system on it:
//
//	hdiskN  ->  VG "pv<wwn suffix>"  ->  LV "lv<wwn suffix>"  ->  jfs2 (inline log)
//	                                     LV "loglvNN"           (jfs only: its log)
//
// Keeping the whole volume group on the one disk makes the volume portable.
// The volume group descriptor area (VGDA) travels with the disk, so when the
// volume moves to another node, NodeStageVolume imports the volume group there
// and the file system comes with it. NodeUnstageVolume varies the volume group
// off and exports it, which forgets it on this node without touching the disk.
//
// The volume group is created with automatic varyon at boot turned off, so a
// rebooted node does not grab a volume that has since moved elsewhere.
//
// Nothing here formats a disk that holds data it does not recognise: a disk
// with no VGDA is only given a volume group if its first 64 KiB are blank
// (an AIX IPL record carrying just a PVID is allowed).
//
// Commands run through a Runner, so the sequencing is unit-tested on Linux
// against canned AIX output. The command output formats are from the AIX 7.3
// documentation and have not yet been checked on hardware.
package aixlvm
