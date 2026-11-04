# Running the node plugin on AIX

The node plugin runs on IBM AIX (`GOOS=aix GOARCH=ppc64`, cgo-free) and makes
JFS2 or JFS file systems on PowerVS volumes. The controller, the provisioner
and the other sidecars keep running on Linux, unchanged. Linux node behaviour
is unchanged too.

Nothing here has been run on AIX hardware yet. See [Not yet verified](#not-yet-verified).

## Why it is not a DaemonSet

On Linux the node plugin is a privileged DaemonSet pod. On AIX it runs on the
host as an SRC subsystem, `csi-powervs`, in group `kubernetes` (like the
kubelet), started at boot from `/etc/inittab`. Two things a pod would need do
not exist on AIX:

- **Mount propagation.** The plugin mounts file systems under
  `/var/lib/kubelet`, which the kubelet must see. From a container that needs
  `mountPropagation: Bidirectional`. AIX has no mount namespaces or
  propagation, and containerd-aix rejects every propagation mode except
  `private` (`internal/wpar/fs_isolation.go`).
- **Device access.** Finding and configuring the volume's disk (`cfgmgr`,
  `odmget`) and building a volume group on it (`mkvg`, `importvg`) need the
  global environment. containerd-aix WPARs get no devices.

The plugin registers itself with the kubelet (`--kubelet-registration-path`),
serving the plugin registration API on
`/var/lib/kubelet/plugins_registry/powervs.csi.ibm.com-reg.sock`, so there is
no `node-driver-registrar`. It removes both sockets on SIGTERM (`stopsrc`) and
exits 1 when the kubelet rejects the registration, so SRC restarts it.

The Linux DaemonSet already selects `kubernetes.io/os: linux` and
`kubernetes.io/arch: ppc64le`, so it never lands on AIX nodes.

## How a volume is laid out

AIX does not put a file system straight on a disk; JFS and JFS2 live on a
logical volume. Each CSI volume therefore gets a volume group of its own:

```
hdiskN  ->  VG pvs<12 hex>  ->  LV pvl<12 hex>  ->  jfs2 (inline log)
                                LV loglvNN          (jfs only: its jfslog)
```

The 12 hex digits are the start of the SHA-256 of the volume's WWN. They are
not the WWN's own trailing digits, because IBM storage WWNs end in a per-
controller sequence number, so two volumes from different controllers can
share them. AIX allows 15 characters for volume group and logical volume
names; `pvs`/`pvl` plus 12 digits fit.

Because the whole volume group sits on one disk, its descriptor area (VGDA)
travels with the volume. A volume that moves to another node is imported there
with its file system intact.

## What each call does

| CSI call | AIX |
|---|---|
| NodeStageVolume | Find the hdisk whose ODM `unique_id` contains the WWN (`odmget -q attribute=unique_id CuAt`, `lsdev`, `lspath`); if it is not there, `cfgmgr -l` each `fcs`/`vscsi` adapter and retry, as the Linux code rescans SCSI hosts. Then prepare the file system (below) and `mount -v jfs2 -o log=INLINE,… /dev/pvl… <staging>` (for JFS, `log=` names its jfslog). |
| NodePublishVolume | `mount -v namefs [-o ro] <staging> <target>`: namefs is AIX's bind mount. containerd-aix grafts the target into the pod's WPAR. |
| NodeUnpublishVolume | `umount <target>` |
| NodeUnstageVolume | `umount <staging>`, `varyoffvg`, `exportvg` (forgets the volume group on this node, leaves it on the disk), `rmdev -dl hdiskN`, so no stale paths remain when the controller detaches the volume. If an earlier attempt stopped after the unmount, the retry finds the volume group through the staging path's `/etc/filesystems` stanza and finishes the job, even if the disk has lost its paths. |
| NodeExpandVolume | `chvg -g` (pick up the larger disk; its failure when the size has not changed is only logged, so a retry still gets through), `chlv -x` to raise the logical volume's partition limit, then `chfs -a size=+<free>M` on the staging mount, which grows the logical volume and the file system online. The volume path may be the pod's namefs mount; it is followed back to the logical volume. |
| NodeGetVolumeStats | `statfs` |

Preparing the file system at stage time:

1. **Disk already in this volume's group** (restaged on the same node): vary it
   on if needed.
2. **Disk carries a VGDA** (`lqueryvg -p hdiskN -v` succeeds and prints a
   non-zero volume group ID) but is not imported: `importvg -y pvs…`. The
   group must hold this volume's logical volume `pvl…` and nothing but logs
   besides; otherwise it is someone else's data, and it is varied off and
   exported again before the error is returned, leaving it as found. Only
   then `chvg -a n`, so it is not varied on at boot.
   - **Clones.** A clone of a volume carries its source's logical volume,
     `pvl<source>`. A group holding exactly one driver-made logical volume
     (and logs) is accepted: the stanza importvg made for the old name is
     dropped (`imfs -x -l`) and the logical volume renamed with `chlv -n`.
3. **No VGDA**: the first 1 MiB of `/dev/rhdiskN` must be zero (an AIX IPL
   record that only carries a PVID is allowed); a short read is an error.
   Anything else (an ext4, XFS, Btrfs or LVM2 signature from a volume used on
   Linux, a partition table) is refused rather than formatted. Then
   `mkvg -S -n -s 64 -P 512` (scalable, no varyon at boot, 64 MB partitions,
   room to grow to 32 TB; left to choose, mkvg sizes partitions for the disk's
   current size and caps how far a small volume can later grow), `mklv -t
   jfs2|jfs -x <n>` over all free partitions (one fewer for JFS, for its log)
   and `crfs -d <lv> -A no` (JFS2 with `logname=INLINE`; for JFS, crfs makes a
   jfslog in the same group). A crfs failure removes the logical volume again.
   If that removal fails too, the next attempt finds the logical volume with
   no `/etc/filesystems` entry and runs crfs on it, if its first 1 MiB is
   still blank.
4. Make sure `/etc/filesystems` mounts `/dev/pvl…` at the staging path, for
   chfs to grow it later. crfs writes the stanza and importvg rebuilds it from
   the logical volume's label; a stanza with another mount point is moved with
   `chfs -m`, a missing one is recreated with `chlv -L` and `imfs -l`.

LVM changes, `cfgmgr` and `rmdev` are serialised in the plugin: AIX LVM and
ODM updates take global locks, and parallel `mkvg`/`importvg` calls fail on
them. The commands that change LVM state run without a timeout, since killing
one halfway can leave the ODM and the VGDA out of step. On SIGTERM the plugin
removes its sockets, then waits for a change in progress to finish before it
exits; the SRC subsystem allows it 600 seconds before SIGKILL.

The plugin only ever varies off, exports or grows volume groups whose names it
generates (`pvs` + 12 hex digits), so `rootvg` and an administrator's own
volume groups are never touched. A volume whose disk is in such a group fails
to stage with an error naming the group.

## File systems and access modes

| | Linux nodes | AIX nodes |
|---|---|---|
| fsType | ext2, ext3, ext4 (default), xfs | jfs2 (default), jfs |
| Filesystem volumes | single- and multi-node modes | single-node modes only |
| Raw block volumes | yes | refused |

Multi-node access is refused on AIX because JFS and JFS2 are not cluster file
systems, and varying on the volume group on a second node writes to its VGDA
even for a read-only mount. Raw block volumes are refused because the AIX
kubelet has no block volume path handler, and AIX has no bind mount to publish
a device node with. Both return `InvalidArgument`.

JFS is limited to 1 TB per file system; use JFS2 for anything large.

A volume made on AIX cannot be mounted by a Linux node, nor the reverse:
the PV records its fsType, and each node refuses the other's file systems.
A statically provisioned PV with no fsType is the exception. On a Linux node
it would default to ext4, and blkid probably does not recognise AIX LVM, so
an AIX volume could be formatted. Always set fsType on PVs for AIX volumes,
and keep AIX workloads on AIX nodes with node affinity. The node topology
does not yet carry the OS.

`deploy/aix/storageclass-aix.yaml` has a StorageClass for each. `mountOptions`
are passed to the AIX `mount` command (`noatime`, `rbrw`, `cio`, …).

## Installing

Prerequisites on the node:

- AIX 7.2 or later, with the volume attached through NPIV (`fcs` adapters) or
  vSCSI and AIX MPIO managing the disks (the default for IBM storage).
- An IBM Cloud API key that can read the PowerVS workspace, in a file only root
  can read. Write it without a trailing newline (`printf '%s' "$KEY" >
  file`), since the plugin uses the file's contents as they are.
- Either a kubeconfig the plugin can read its Node object with (the kubelet's
  own works: the node authorizer allows it) so it can parse the providerID
  `ibmpowervs://<region>/<zone>/<workspace>/<vm>`, or the workspace, zone and
  VM given directly as `POWERVS_CLOUD_INSTANCE_ID`, `POWERVS_ZONE` and
  `POWERVS_INSTANCE_ID`.

Build and install:

```sh
# on a Linux build host
CGO_ENABLED=0 GOOS=aix GOARCH=ppc64 make driver

# on the AIX node
mkdir -p /opt/ibm-powervs-block-csi-driver/bin /etc/ibm-powervs-csi
cp ibm-powervs-block-csi-driver /opt/ibm-powervs-block-csi-driver/bin/
printf '%s' "$IBMCLOUD_API_KEY" > /etc/ibm-powervs-csi/IBMCLOUD_API_KEY
chmod 600 /etc/ibm-powervs-csi/IBMCLOUD_API_KEY
KUBECONFIG=/path/to/kubelet-kubeconfig ./deploy/aix/install-aix-node.sh
```

SRC does not pass environment variables, so the installer writes them to
`/etc/ibm-powervs-csi/node.env` (mode 0600) and registers a small wrapper,
`powervs-csi-node`, that loads them and runs the plugin. `lssrc -s
csi-powervs` shows its state; logs go to `/var/log/csi-powervs.log`.
`deploy/aix/uninstall-aix-node.sh` removes it (drain the node first).

## Blocker: the kubelet plugin watcher

The AIX kubelet port has no plugin watcher yet: fsnotify has no AIX backend,
so the kubelet never notices the registration socket. Until it gets one
(AHAFS-backed or polling), the plugin starts and serves CSI, but the kubelet
never calls it. The NFS CSI port and TopoLVM on AIX wait on the same thing.

## Known limits

- **A clone on the same node as its source** cannot be staged while the
  source is imported there: the clone's disk carries the same PVID and
  volume group ID. `recreatevg`, which gives the copy new IDs, would fix this
  and is not done yet.
- **The disk is not re-checked against the request's WWN** between finding it
  and preparing it. Disk numbers change only through `cfgmgr` and `rmdev`,
  which run under the same lock as the LVM changes, so a mix-up needs the
  volume's own disk to be removed and its number reused in between.

## Not yet verified

Everything below is from the AIX documentation and was checked only against
canned output in unit tests. To check first on a PowerVS AIX LPAR:

- The `unique_id` of a PowerVS volume contains the WWN PowerVS reports (the
  parser expects `33213` + 32 hex digits + `04214503IBMfcp` for SVC/FlashSystem
  volumes, and matches by substring).
- Output formats of `lspv`, `lsvg`, `lsvg -l`, `lsvg -p`, `lslv`, `lsfs -c`,
  `lsdev -F`, `lspath -F`, `odmget`, and `mount` with a jfs2 and a namefs row.
- **Staging path length.** The kubelet's staging path is about 140
  characters. If the logical volume label is shorter (reportedly about 127),
  `crfs -m` or `chlv -L` may fail or truncate it. A truncated label only
  matters after `importvg` on another node, where the stanza is then moved
  with `chfs -m`; a failure would break every stage. Check this first.
- `lqueryvg -p hdiskN -v` exits non-zero, or prints a zero ID, on a disk with
  no VGDA.
- `mkvg -S -s 64 -P 512` on a small (1 GB) volume: the VGDA for 512K
  partitions must leave room for data.
- `chvg -g` exiting non-zero when the disk has not grown, and `chlv -x` then
  `chfs -a size=+NM` growing JFS2 online.
- `imfs -x -l <old>` then `chlv -n` on a cloned logical volume, leaving one
  stanza for the staging path.
- `crfs -v jfs2 -d <lv> -a logname=INLINE` on an existing logical volume, and
  `crfs -v jfs -d <lv>` creating its jfslog in the same group.
- `importvg` recreating the stanza from the logical volume label; `imfs -l`
  for a missing one.
- `statfs` block units: the plugin counts in `f_fsize` (the fundamental block
  size) and falls back to `f_bsize`. Compare `NodeGetVolumeStats` with `df -k`
  on a JFS file system made with a fragment size under 4 KiB.
- `cfgmgr -l fcsN` finding a newly attached volume, and `rmdev -dl` before
  detach leaving no Missing paths.

## Development

The AIX code keeps out of the shared files where it can: the Linux device code
is tagged `!aix`, and the OS-specific choices sit in
`pkg/driver/node_platform_default.go` and `node_platform_aix.go`. Parsers and
the LVM sequencing (`pkg/aixlvm`, run through a fake command runner) are
unit-tested on Linux:

```sh
go test ./pkg/... ./cmd/...
GOOS=aix GOARCH=ppc64 CGO_ENABLED=0 go build ./cmd/... ./pkg/...
```

`go vet` for aix on `pkg/driver` fails in the test dependencies (csi-test pulls
in ginkgo v1, which does not build on AIX); vetting the package without its
tests passes.

`pkg/aixmount` is copied unchanged from
[gilbahat/csi-driver-nfs](https://github.com/gilbahat/csi-driver-nfs) and
should stay in step with it until it moves to a shared module.
