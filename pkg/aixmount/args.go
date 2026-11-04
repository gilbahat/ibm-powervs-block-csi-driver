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
)

const maskedOption = "<masked>"

// mountCommand is the AIX mount command line for one request.
type mountCommand struct {
	// Args are the arguments to /usr/sbin/mount.
	Args []string
	// LogArgs are Args with sensitive options masked.
	LogArgs []string
	// Translation records how NFS options were rewritten, if they were.
	Translation TranslatedOptions
}

// isNFSRequest reports whether a mount request is for an NFS share.
func isNFSRequest(source, fstype string) bool {
	switch strings.ToLower(fstype) {
	case "nfs", "nfs3", "nfs4":
		return true
	case "":
		// "host:/path" with no fstype, as Linux mount(8) also accepts.
		host, path, ok := strings.Cut(source, ":")
		return ok && host != "" && strings.HasPrefix(path, "/")
	}
	return false
}

// buildMountCommand turns a mount-utils style request into AIX mount
// arguments:
//
//   - NFS: options are translated by TranslateNFSOptions and no -v flag is
//     passed, because AIX picks the nfs, nfs3 or nfs4 vfs from vers=.
//     fstype nfs3 or nfs4 implies vers=3 or vers=4 when vers= is absent.
//   - Bind mounts (a "bind" or "rbind" option) become namefs mounts, which
//     take ro directly, so no remount step is needed as on Linux.
//   - Anything else is passed through as `mount -v <fstype>`.
func buildMountCommand(source, target, fstype string, options, sensitiveOptions []string, policy NFSOptionPolicy) (mountCommand, error) {
	var cmd mountCommand
	if source == "" || target == "" {
		return cmd, optionErrorf("mount source and target are required")
	}
	for _, o := range options {
		if strings.EqualFold(strings.TrimSpace(o), "remount") {
			return cmd, optionErrorf("remount is not supported on AIX")
		}
	}

	var vfs string
	opts := options

	switch {
	case isNFSRequest(source, fstype):
		t, err := TranslateNFSOptions(options, policy)
		if err != nil {
			return cmd, err
		}
		cmd.Translation = t
		opts = t.Options
		if !hasOptionKey(opts, "vers") {
			switch strings.ToLower(fstype) {
			case "nfs3":
				opts = append(opts, "vers=3")
			case "nfs4":
				opts = append(opts, "vers=4")
			}
		}

	case hasOption(options, "bind") || hasOption(options, "rbind"):
		vfs = "namefs"
		opts = nil
		for _, group := range options {
			for _, o := range strings.Split(group, ",") {
				o = strings.TrimSpace(o)
				if o == "" || o == "bind" || o == "rbind" {
					continue
				}
				opts = append(opts, o)
			}
		}

	default:
		if fstype == "" {
			return cmd, optionErrorf("fstype is required to mount %s on AIX", source)
		}
		vfs = fstype
	}

	var args, logArgs []string
	if vfs != "" {
		args = append(args, "-v", vfs)
		logArgs = append(logArgs, "-v", vfs)
	}
	allOpts := append(append([]string{}, opts...), sensitiveOptions...)
	if len(allOpts) > 0 {
		args = append(args, "-o", strings.Join(allOpts, ","))
		logOpts := append([]string{}, opts...)
		for range sensitiveOptions {
			logOpts = append(logOpts, maskedOption)
		}
		logArgs = append(logArgs, "-o", strings.Join(logOpts, ","))
	}
	args = append(args, source, target)
	logArgs = append(logArgs, source, target)

	cmd.Args, cmd.LogArgs = args, logArgs
	return cmd, nil
}

func hasOption(opts []string, want string) bool {
	for _, o := range opts {
		for _, p := range strings.Split(o, ",") {
			if strings.TrimSpace(p) == want {
				return true
			}
		}
	}
	return false
}

func hasOptionKey(opts []string, key string) bool {
	for _, o := range opts {
		k, _, _ := strings.Cut(o, "=")
		if k == key {
			return true
		}
	}
	return false
}
