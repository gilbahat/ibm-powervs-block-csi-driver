//go:build aix || linux

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

// The Mounter is meant for AIX. It also builds on Linux only so that its
// command handling can be unit-tested there against fake mount and umount
// scripts that print AIX output.

package aixmount

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"k8s.io/klog/v2"
	mount "k8s.io/mount-utils"
)

const (
	// DefaultMountPath and DefaultUmountPath are the AIX commands.
	DefaultMountPath  = "/usr/sbin/mount"
	DefaultUmountPath = "/usr/sbin/umount"

	// killGracePeriod is how long to wait for a killed command to exit.
	killGracePeriod = 2 * time.Second
)

// Mounter implements mount.Interface and mount.MounterForceUnmounter on AIX.
type Mounter struct {
	// MountPath and UmountPath override the command paths (for tests).
	MountPath  string
	UmountPath string
	// NFS sets how Linux NFS options are translated.
	NFS NFSOptionPolicy
}

var _ mount.MounterForceUnmounter = &Mounter{}

// New returns a Mounter using the AIX mount commands.
func New(policy NFSOptionPolicy) *Mounter {
	return &Mounter{MountPath: DefaultMountPath, UmountPath: DefaultUmountPath, NFS: policy}
}

// Mount mounts source on target.
func (m *Mounter) Mount(source, target, fstype string, options []string) error {
	return m.MountContext(context.Background(), source, target, fstype, options, nil)
}

// MountSensitive is Mount with options that are masked in logs.
func (m *Mounter) MountSensitive(source, target, fstype string, options, sensitiveOptions []string) error {
	return m.MountContext(context.Background(), source, target, fstype, options, sensitiveOptions)
}

// MountSensitiveWithoutSystemd is MountSensitive: AIX has no systemd.
func (m *Mounter) MountSensitiveWithoutSystemd(source, target, fstype string, options, sensitiveOptions []string) error {
	return m.MountContext(context.Background(), source, target, fstype, options, sensitiveOptions)
}

// MountSensitiveWithoutSystemdWithMountFlags is MountSensitive. The mount
// flags are Linux mount(8) flags such as --no-canonicalize, which the AIX
// mount command does not take, so they are ignored.
func (m *Mounter) MountSensitiveWithoutSystemdWithMountFlags(source, target, fstype string, options, sensitiveOptions, mountFlags []string) error {
	if len(mountFlags) > 0 {
		klog.V(4).Infof("aixmount: ignoring Linux mount flags %v", mountFlags)
	}
	return m.MountContext(context.Background(), source, target, fstype, options, sensitiveOptions)
}

// MountContext mounts source on target, killing the mount command if ctx is
// done first. A hung NFS server otherwise leaves the command blocked for as
// long as the AIX client keeps retrying.
func (m *Mounter) MountContext(ctx context.Context, source, target, fstype string, options, sensitiveOptions []string) error {
	cmd, err := buildMountCommand(source, target, fstype, options, sensitiveOptions, m.NFS)
	if err != nil {
		return err
	}
	if len(cmd.Translation.Dropped) > 0 {
		klog.V(2).Infof("aixmount: dropped Linux-only NFS options %v for %s", cmd.Translation.Dropped, target)
	}
	for _, note := range cmd.Translation.Notes {
		klog.Warningf("aixmount: %s: %s", target, note)
	}
	logArgs := strings.Join(cmd.LogArgs, " ")
	klog.V(4).Infof("aixmount: %s %s", m.mountPath(), logArgs)

	out, err := run(ctx, m.mountPath(), cmd.Args...)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return fmt.Errorf("mount %s on %s: %w", source, target, err)
		}
		return fmt.Errorf("mount failed: %v\nMounting command: %s\nMounting arguments: %s\nOutput: %s",
			err, m.mountPath(), logArgs, strings.TrimSpace(string(out)))
	}
	return nil
}

// Unmount unmounts target.
func (m *Mounter) Unmount(target string) error {
	return m.unmount(context.Background(), target, false)
}

// UnmountWithForce unmounts target, retrying with umount -f if the plain
// unmount has not finished within umountTimeout. As on Linux, -f is how a
// mount whose NFS server has gone away gets released.
func (m *Mounter) UnmountWithForce(target string, umountTimeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), umountTimeout)
	defer cancel()
	err := m.unmount(ctx, target, false)
	if err != nil && errors.Is(err, context.DeadlineExceeded) {
		klog.V(2).Infof("aixmount: timed out unmounting %s, retrying with -f", target)
		return m.unmount(context.Background(), target, true)
	}
	return err
}

func (m *Mounter) unmount(ctx context.Context, target string, force bool) error {
	args := []string{target}
	if force {
		args = []string{"-f", target}
	}
	klog.V(4).Infof("aixmount: %s %s", m.umountPath(), strings.Join(args, " "))
	out, err := run(ctx, m.umountPath(), args...)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return err
		}
		return fmt.Errorf("unmount failed: %v\nUnmounting arguments: %s\nOutput: %s",
			err, strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return nil
}

// List returns the mount table.
func (m *Mounter) List() ([]mount.MountPoint, error) {
	out, err := run(context.Background(), m.mountPath())
	if err != nil {
		return nil, fmt.Errorf("listing mounts with %s: %v: %s", m.mountPath(), err, strings.TrimSpace(string(out)))
	}
	return ParseMountTable(string(out)), nil
}

// IsLikelyNotMountPoint reports whether file is not a mount point by
// comparing its device number with its parent's, as the Linux mounter does.
// It is fast but cannot see namefs mounts of a directory on the same file
// system; IsMountPoint can.
func (m *Mounter) IsLikelyNotMountPoint(file string) (bool, error) {
	st, err := os.Stat(file)
	if err != nil {
		return true, err
	}
	rootSt, err := os.Stat(filepath.Dir(strings.TrimSuffix(file, "/")))
	if err != nil {
		return true, err
	}
	if devOf(st) != devOf(rootSt) {
		return false, nil
	}
	return true, nil
}

// CanSafelySkipMountPointCheck is false: AIX umount does not tolerate an
// unmounted target, so callers must check first.
func (m *Mounter) CanSafelySkipMountPointCheck() bool {
	return false
}

// IsMountPoint reports whether file is a mount point, from the mount table.
func (m *Mounter) IsMountPoint(file string) (bool, error) {
	resolved, err := filepath.EvalSymlinks(file)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, fs.ErrNotExist
		}
		return false, err
	}
	mps, err := m.List()
	if err != nil {
		return false, err
	}
	for _, mp := range mps {
		if mp.Path == resolved || mp.Path == file {
			return true, nil
		}
	}
	return false, nil
}

// GetMountRefs returns the other paths where the file system mounted at
// pathname is also mounted, including namefs mounts of pathname itself:
// containerd-aix grafts a pod's volumes into a WPAR with namefs, so those
// show up here while the pod is running.
func (m *Mounter) GetMountRefs(pathname string) ([]string, error) {
	if _, err := os.Stat(pathname); errors.Is(err, fs.ErrNotExist) {
		return []string{}, nil
	} else if err != nil && !mount.IsCorruptedMnt(err) {
		return nil, err
	}
	mps, err := m.List()
	if err != nil {
		return nil, err
	}
	return mountRefs(mps, pathname), nil
}

func (m *Mounter) mountPath() string {
	if m.MountPath != "" {
		return m.MountPath
	}
	return DefaultMountPath
}

func (m *Mounter) umountPath() string {
	if m.UmountPath != "" {
		return m.UmountPath
	}
	return DefaultUmountPath
}

func devOf(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Dev) //nolint:unconvert // Dev's type differs by platform
	}
	return 0
}

// run executes a command in its own process group so that a hung NFS mount
// helper can be killed together with its children when ctx is done. It
// returns the combined output.
//
// WaitDelay bounds how long Wait blocks after the command exits (or is
// killed) while a child still holds its output pipe open, and the output is
// read only after Wait has returned, so the buffer is no longer written to.
func run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = killGracePeriod
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err := cmd.Run()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return out.Bytes(), ctxErr
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		// The command itself succeeded; a leftover child kept the pipe open.
		klog.V(4).Infof("aixmount: %s exited but its output pipe stayed open", name)
		return out.Bytes(), nil
	}
	return out.Bytes(), err
}
