//go:build linux

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
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeCommands writes fake mount and umount scripts into a temp dir. Each
// appends its arguments to a log file; mount with no arguments prints table.
func fakeCommands(t *testing.T, table string, mountBody, umountBody string) (*Mounter, string) {
	t.Helper()
	dir := t.TempDir()
	logFile := filepath.Join(dir, "calls.log")
	tableFile := filepath.Join(dir, "table.txt")
	if err := os.WriteFile(tableFile, []byte(table), 0o644); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		script := "#!/bin/sh\necho \"" + name + " $*\" >> " + logFile + "\n" + body + "\n"
		if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	mountScript := write("mount", "if [ $# -eq 0 ]; then cat "+tableFile+"; exit 0; fi\n"+mountBody)
	umountScript := write("umount", umountBody)
	return &Mounter{MountPath: mountScript, UmountPath: umountScript}, logFile
}

func calls(t *testing.T, logFile string) []string {
	t.Helper()
	b, err := os.ReadFile(logFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func TestMounterMountNFS(t *testing.T) {
	m, log := fakeCommands(t, sampleMountOutput, "exit 0", "exit 0")
	if err := m.Mount("nfssrv:/export", "/mnt/x", "nfs", []string{"nfsvers=3", "hard", "nconnect=4"}); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	if got, want := calls(t, log), []string{"mount -o vers=3,hard nfssrv:/export /mnt/x"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("calls = %q, want %q", got, want)
	}
}

func TestMounterMountFailureIncludesOutput(t *testing.T) {
	m, _ := fakeCommands(t, sampleMountOutput, "echo 'mount: 1831-008 giving up on:' >&2; exit 1", "exit 0")
	err := m.Mount("nfssrv:/export", "/mnt/x", "nfs", nil)
	if err == nil || !strings.Contains(err.Error(), "1831-008 giving up") {
		t.Fatalf("error = %v, want the mount command's output", err)
	}
}

func TestMounterMountRejectedBeforeRunning(t *testing.T) {
	m, log := fakeCommands(t, sampleMountOutput, "exit 0", "exit 0")
	if err := m.Mount("nfssrv:/export", "/mnt/x", "nfs", []string{"nfsvers=4.1"}); err == nil {
		t.Fatal("expected NFSv4.1 to be refused")
	}
	if got := calls(t, log); len(got) != 0 {
		t.Errorf("mount ran anyway: %q", got)
	}
}

func TestMounterMountContextTimeoutKillsCommand(t *testing.T) {
	m, _ := fakeCommands(t, sampleMountOutput, "sleep 30", "exit 0")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := m.MountContext(ctx, "nfssrv:/export", "/mnt/x", "nfs", nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("mount was not killed promptly: %v", elapsed)
	}
}

func TestMounterUnmountWithForce(t *testing.T) {
	// Plain umount hangs; umount -f succeeds.
	m, log := fakeCommands(t, sampleMountOutput, "exit 0", `if [ "$1" = "-f" ]; then exit 0; fi; sleep 30`)
	if err := m.UnmountWithForce("/mnt/x", 200*time.Millisecond); err != nil {
		t.Fatalf("UnmountWithForce: %v", err)
	}
	if got, want := calls(t, log), []string{"umount /mnt/x", "umount -f /mnt/x"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("calls = %q, want %q", got, want)
	}
}

func TestMounterUnmountFastPathNoForce(t *testing.T) {
	m, log := fakeCommands(t, sampleMountOutput, "exit 0", "exit 0")
	if err := m.UnmountWithForce("/mnt/x", time.Second); err != nil {
		t.Fatalf("UnmountWithForce: %v", err)
	}
	if got := calls(t, log); len(got) != 1 || got[0] != "umount /mnt/x" {
		t.Errorf("calls = %q, want a single plain umount", got)
	}
}

func TestMounterListAndIsMountPoint(t *testing.T) {
	m, _ := fakeCommands(t, sampleMountOutput, "exit 0", "exit 0")
	mps, err := m.List()
	if err != nil || len(mps) != 7 {
		t.Fatalf("List = %d entries, %v; want 7", len(mps), err)
	}
	// "/" exists on the test host and is in the fake table.
	if isMnt, err := m.IsMountPoint("/"); err != nil || !isMnt {
		t.Errorf("IsMountPoint(/) = %v, %v; want true", isMnt, err)
	}
	dir := t.TempDir()
	if isMnt, err := m.IsMountPoint(dir); err != nil || isMnt {
		t.Errorf("IsMountPoint(%s) = %v, %v; want false", dir, isMnt, err)
	}
	if _, err := m.IsMountPoint(filepath.Join(dir, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("IsMountPoint(missing) error = %v, want not-exist", err)
	}
}

func TestMounterIsLikelyNotMountPoint(t *testing.T) {
	m := New(NFSOptionPolicy{})
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if notMnt, err := m.IsLikelyNotMountPoint(sub); err != nil || !notMnt {
		t.Errorf("IsLikelyNotMountPoint(sub) = %v, %v; want true", notMnt, err)
	}
	if _, err := m.IsLikelyNotMountPoint(filepath.Join(dir, "missing")); !os.IsNotExist(err) {
		t.Errorf("missing path error = %v, want not-exist", err)
	}
}

func TestMounterGetMountRefs(t *testing.T) {
	m, _ := fakeCommands(t, sampleMountOutput, "exit 0", "exit 0")
	refs, err := m.GetMountRefs(filepath.Join(t.TempDir(), "missing"))
	if err != nil || len(refs) != 0 {
		t.Errorf("GetMountRefs(missing) = %v, %v; want empty", refs, err)
	}
}
