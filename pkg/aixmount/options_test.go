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
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestTranslateNFSOptions(t *testing.T) {
	tests := []struct {
		name        string
		in          []string
		policy      NFSOptionPolicy
		wantOpts    []string
		wantDropped []string
		wantErr     string
		wantNote    bool
	}{
		{
			name:     "nothing",
			in:       nil,
			wantOpts: nil,
		},
		{
			name:     "common linux options pass through",
			in:       []string{"hard", "timeo=600", "retrans=2", "rsize=1048576", "wsize=1048576", "proto=tcp"},
			wantOpts: []string{"hard", "timeo=600", "retrans=2", "rsize=1048576", "wsize=1048576", "proto=tcp"},
		},
		{
			name:     "nfsvers=3 becomes vers=3",
			in:       []string{"nfsvers=3"},
			wantOpts: []string{"vers=3"},
		},
		{
			name:     "vers=4.0 becomes vers=4",
			in:       []string{"vers=4.0", "hard"},
			wantOpts: []string{"vers=4", "hard"},
		},
		{
			name:    "nfsvers=4.1 is refused by default",
			in:      []string{"nfsvers=4.1"},
			wantErr: "NFSv4.1 is not supported by the AIX NFS client",
		},
		{
			name:     "nfsvers=4.1 falls back when allowed",
			in:       []string{"nfsvers=4.1"},
			policy:   NFSOptionPolicy{NFS4MinorFallback: true},
			wantOpts: []string{"vers=4"},
			wantNote: true,
		},
		{
			name:    "minorversion=1 is refused by default",
			in:      []string{"vers=4", "minorversion=1"},
			wantErr: "NFSv4.1 is not supported",
		},
		{
			name:     "minorversion=0 is just v4",
			in:       []string{"vers=4", "minorversion=0"},
			wantOpts: []string{"vers=4"},
		},
		{
			name:    "conflicting versions",
			in:      []string{"vers=3", "nfsvers=4"},
			wantErr: "conflicting NFS versions",
		},
		{
			name:    "unknown version",
			in:      []string{"vers=5"},
			wantErr: "unsupported NFS version",
		},
		{
			name:        "comma-joined groups from the mountOptions attribute",
			in:          []string{"hard", "nfsvers=3,nolock, noresvport"},
			wantOpts:    []string{"hard", "vers=3", "llock"},
			wantDropped: []string{"noresvport"},
		},
		{
			name:        "linux-only tuning is dropped",
			in:          []string{"nconnect=8", "lookupcache=positive", "noatime", "_netdev", "x-systemd.automount", "hard"},
			wantOpts:    []string{"hard"},
			wantDropped: []string{"nconnect=8", "lookupcache=positive", "noatime", "_netdev", "x-systemd.automount"},
		},
		{
			name:     "tcp shorthand",
			in:       []string{"tcp"},
			wantOpts: []string{"proto=tcp"},
		},
		{
			name:        "bg is dropped",
			in:          []string{"bg", "hard"},
			wantOpts:    []string{"hard"},
			wantDropped: []string{"bg"},
			wantNote:    true,
		},
		{
			name:     "access control options are never dropped",
			in:       []string{"ro", "nosuid", "nodev", "noexec", "sec=krb5p"},
			wantOpts: []string{"ro", "nosuid", "nodev", "noexec", "sec=krb5p"},
		},
		{
			name:     "unknown options pass through for mount to judge",
			in:       []string{"somethingnew=1"},
			wantOpts: []string{"somethingnew=1"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := TranslateNFSOptions(tc.in, tc.policy)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want one containing %q", err, tc.wantErr)
				}
				var optErr *OptionError
				if !errors.As(err, &optErr) {
					t.Errorf("error %v is not an *OptionError", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got.Options, tc.wantOpts) {
				t.Errorf("Options = %q, want %q", got.Options, tc.wantOpts)
			}
			if !reflect.DeepEqual(got.Dropped, tc.wantDropped) {
				t.Errorf("Dropped = %q, want %q", got.Dropped, tc.wantDropped)
			}
			if tc.wantNote != (len(got.Notes) > 0) {
				t.Errorf("Notes = %q, want notes: %v", got.Notes, tc.wantNote)
			}
		})
	}
}

func TestBuildMountCommand(t *testing.T) {
	tests := []struct {
		name      string
		source    string
		target    string
		fstype    string
		opts      []string
		sensitive []string
		wantArgs  []string
		wantLog   []string
		wantErr   string
	}{
		{
			name:     "nfs share, no -v",
			source:   "nfssrv:/export/data",
			target:   "/mnt/t",
			fstype:   "nfs",
			opts:     []string{"nfsvers=3", "hard"},
			wantArgs: []string{"-o", "vers=3,hard", "nfssrv:/export/data", "/mnt/t"},
		},
		{
			name:     "nfs4 fstype implies vers=4",
			source:   "nfssrv:/export",
			target:   "/mnt/t",
			fstype:   "nfs4",
			wantArgs: []string{"-o", "vers=4", "nfssrv:/export", "/mnt/t"},
		},
		{
			name:     "nfs with no options",
			source:   "10.0.0.5:/export",
			target:   "/mnt/t",
			fstype:   "nfs",
			wantArgs: []string{"10.0.0.5:/export", "/mnt/t"},
		},
		{
			name:     "host:/path with no fstype is nfs",
			source:   "nfssrv:/export",
			target:   "/mnt/t",
			opts:     []string{"nolock"},
			wantArgs: []string{"-o", "llock", "nfssrv:/export", "/mnt/t"},
		},
		{
			name:     "bind becomes namefs",
			source:   "/var/lib/kubelet/pods/p/volumes/v",
			target:   "/wpars/c/data",
			opts:     []string{"bind", "ro"},
			wantArgs: []string{"-v", "namefs", "-o", "ro", "/var/lib/kubelet/pods/p/volumes/v", "/wpars/c/data"},
		},
		{
			name:     "comma-joined bind options",
			source:   "/a",
			target:   "/b",
			opts:     []string{"bind,ro"},
			wantArgs: []string{"-v", "namefs", "-o", "ro", "/a", "/b"},
		},
		{
			name:     "other fstypes pass through",
			source:   "/dev/fslv01",
			target:   "/data",
			fstype:   "jfs2",
			opts:     []string{"log=INLINE"},
			wantArgs: []string{"-v", "jfs2", "-o", "log=INLINE", "/dev/fslv01", "/data"},
		},
		{
			name:      "sensitive options are masked in the log",
			source:    "nfssrv:/export",
			target:    "/mnt/t",
			fstype:    "nfs",
			opts:      []string{"hard"},
			sensitive: []string{"secret=x"},
			wantArgs:  []string{"-o", "hard,secret=x", "nfssrv:/export", "/mnt/t"},
			wantLog:   []string{"-o", "hard,<masked>", "nfssrv:/export", "/mnt/t"},
		},
		{
			name:    "remount refused",
			source:  "/a",
			target:  "/b",
			opts:    []string{"bind", "remount", "ro"},
			wantErr: "remount is not supported",
		},
		{
			name:    "local device needs fstype",
			source:  "/dev/fslv01",
			target:  "/data",
			wantErr: "fstype is required",
		},
		{
			name:    "nfsv4.1 refused",
			source:  "nfssrv:/export",
			target:  "/mnt/t",
			fstype:  "nfs",
			opts:    []string{"nfsvers=4.1"},
			wantErr: "NFSv4.1 is not supported",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildMountCommand(tc.source, tc.target, tc.fstype, tc.opts, tc.sensitive, NFSOptionPolicy{})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got.Args, tc.wantArgs) {
				t.Errorf("Args = %q, want %q", got.Args, tc.wantArgs)
			}
			wantLog := tc.wantLog
			if wantLog == nil {
				wantLog = tc.wantArgs
			}
			if !reflect.DeepEqual(got.LogArgs, wantLog) {
				t.Errorf("LogArgs = %q, want %q", got.LogArgs, wantLog)
			}
		})
	}
}
