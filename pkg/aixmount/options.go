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
	"fmt"
	"strconv"
	"strings"
)

// NFSOptionPolicy controls how Linux NFS mount options that AIX cannot honour
// exactly are treated.
type NFSOptionPolicy struct {
	// NFS4MinorFallback mounts NFSv4.1 and v4.2 requests as NFSv4.0. The AIX
	// NFS client speaks 4.0 only (its vers= option takes no minor version),
	// so without this a StorageClass asking for nfsvers=4.1 fails on AIX
	// nodes. With it, an AIX pod and a Linux pod can share one PV, at the
	// cost of 4.1 features such as sessions and pNFS on the AIX side.
	NFS4MinorFallback bool
}

// OptionError is a mount request the AIX mount command cannot express, such
// as an NFSv4.1 version. It is the caller's fault, so CSI callers can report
// it as InvalidArgument rather than Internal.
type OptionError struct{ msg string }

func (e *OptionError) Error() string { return e.msg }

// InvalidArgument marks the error as a bad request.
func (e *OptionError) InvalidArgument() bool { return true }

func optionErrorf(format string, args ...any) error {
	return &OptionError{msg: fmt.Sprintf(format, args...)}
}

// TranslatedOptions is the result of TranslateNFSOptions.
type TranslatedOptions struct {
	// Options are the options to pass to the AIX mount command.
	Options []string
	// Dropped are Linux-only options that were removed. Only tuning and
	// bookkeeping options are ever dropped, never ones that affect access
	// control (ro, nosuid, nodev, noexec, sec=...).
	Dropped []string
	// Notes describe any other change made to the request, for logging.
	Notes []string
}

// droppedLinuxNFSOptions are Linux NFS client options with no AIX
// equivalent whose absence changes performance or caching only. Keys are
// matched before "=".
var droppedLinuxNFSOptions = map[string]bool{
	// Transport tuning. AIX chooses reserved source ports globally with
	// `nfso -o nfs_use_reserved_ports`, not per mount.
	"noresvport": true, "resvport": true, "nconnect": true, "max_connect": true,
	"trunkdiscovery": true, "notrunkdiscovery": true,
	"mountproto": true, "mountport": true, "mounthost": true, "mountvers": true,
	"clientaddr": true, "namlen": true,
	// Caching and directory listing behaviour.
	"lookupcache": true, "local_lock": true, "fsc": true,
	"sharecache": true, "nosharecache": true, "rdirplus": true, "nordirplus": true,
	"cto": true, "nocto": true, "migration": true, "nomigration": true,
	// Access-time tracking, which an NFS client does not control anyway.
	"atime": true, "noatime": true, "diratime": true, "nodiratime": true,
	"relatime": true, "norelatime": true, "strictatime": true, "lazytime": true,
	// fstab and systemd bookkeeping.
	"defaults": true, "auto": true, "noauto": true, "user": true, "nouser": true,
	"nofail": true, "_netdev": true,
	// Locking on: the AIX default.
	"lock": true,
}

// TranslateNFSOptions converts mount options written for the Linux NFS
// client into ones the AIX NFS client accepts.
//
// Each element may hold several comma-separated options, as the driver's
// mountOptions volume attribute does. Options this function does not know
// are passed through unchanged, so a Linux-only option that matters makes
// the AIX mount command fail loudly instead of being ignored.
func TranslateNFSOptions(in []string, policy NFSOptionPolicy) (TranslatedOptions, error) {
	var out TranslatedOptions
	version := ""
	versionIndex := -1

	setVersion := func(requested string, aixVersion string) error {
		if version != "" && version != aixVersion {
			return optionErrorf("conflicting NFS versions requested: vers=%s and %s", version, requested)
		}
		if version == "" {
			versionIndex = len(out.Options)
			out.Options = append(out.Options, "") // placeholder, filled below
		}
		version = aixVersion
		return nil
	}

	for _, group := range in {
		for _, raw := range strings.Split(group, ",") {
			opt := strings.TrimSpace(raw)
			if opt == "" {
				continue
			}
			key, value, hasValue := strings.Cut(opt, "=")
			key = strings.ToLower(key)

			switch {
			case key == "vers" || key == "nfsvers":
				if !hasValue {
					return out, optionErrorf("mount option %q has no value", opt)
				}
				v, note, err := aixNFSVersion(value, policy)
				if err != nil {
					return out, err
				}
				if note != "" {
					out.Notes = append(out.Notes, note)
				}
				if err := setVersion(opt, v); err != nil {
					return out, err
				}

			case key == "minorversion":
				minor, err := strconv.Atoi(value)
				if err != nil || minor < 0 {
					return out, optionErrorf("invalid mount option %q", opt)
				}
				if minor > 0 {
					if !policy.NFS4MinorFallback {
						return out, minorVersionError(fmt.Sprintf("4.%d", minor))
					}
					out.Notes = append(out.Notes, fmt.Sprintf("NFSv4.%d requested; mounting as NFSv4.0 (AIX has no NFSv4.1+ client)", minor))
				}
				if err := setVersion(opt, "4"); err != nil {
					return out, err
				}

			case key == "nolock":
				// AIX: llock keeps file locks local instead of using the NLM.
				out.Options = append(out.Options, "llock")

			case key == "tcp" || key == "udp":
				out.Options = append(out.Options, "proto="+key)

			case key == "bg":
				// A background mount returns before the share is mounted, so
				// the pod would start on an empty directory. CSI needs the
				// publish call to report the real outcome.
				out.Dropped = append(out.Dropped, opt)
				out.Notes = append(out.Notes, "bg ignored: CSI mounts must complete in the foreground")

			case droppedLinuxNFSOptions[key] || strings.HasPrefix(key, "x-"):
				out.Dropped = append(out.Dropped, opt)

			default:
				out.Options = append(out.Options, opt)
			}
		}
	}

	if versionIndex >= 0 {
		out.Options[versionIndex] = "vers=" + version
	}
	return out, nil
}

// aixNFSVersion maps a Linux vers=/nfsvers= value to the AIX vers= value.
func aixNFSVersion(value string, policy NFSOptionPolicy) (string, string, error) {
	switch value {
	case "2", "3", "4":
		return value, "", nil
	case "4.0":
		return "4", "", nil
	case "4.1", "4.2":
		if !policy.NFS4MinorFallback {
			return "", "", minorVersionError(value)
		}
		return "4", fmt.Sprintf("NFSv%s requested; mounting as NFSv4.0 (AIX has no NFSv4.1+ client)", value), nil
	default:
		return "", "", optionErrorf("unsupported NFS version %q on AIX: use 2, 3 or 4", value)
	}
}

func minorVersionError(v string) error {
	return optionErrorf("NFSv%s is not supported by the AIX NFS client, which speaks NFSv4.0 only: "+
		"use nfsvers=4 or nfsvers=3 in a StorageClass for AIX nodes, or start the node plugin with "+
		"--aix-nfs4-minor-fallback to mount NFSv4.1+ volumes as NFSv4.0", v)
}
