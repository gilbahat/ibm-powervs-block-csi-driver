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

// Parsers for the AIX device commands. They are platform-independent so they
// are unit-tested on Linux; device_aix.go runs the commands.

package device

import (
	"regexp"
	"strings"
)

var aixDiskNameRe = regexp.MustCompile(`^hdisk[0-9]+$`)

// aixDisk is a disk known to the AIX ODM.
type aixDisk struct {
	Name     string
	Status   string // Available or Defined
	UniqueID string
}

// parseODMAttr parses `odmget -q "attribute=<attr>" CuAt` into a map from
// device name to value:
//
//	CuAt:
//	        name = "hdisk4"
//	        attribute = "unique_id"
//	        value = "3321360050768108100A8B8000000000003BE04214503IBMfcp"
//	        type = "R"
func parseODMAttr(out string) map[string]string {
	values := map[string]string{}
	var name, value string
	flush := func() {
		if name != "" {
			values[name] = value
		}
		name, value = "", ""
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasSuffix(line, ":") && !strings.Contains(line, "=") {
			flush()
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"`)
		switch strings.TrimSpace(k) {
		case "name":
			name = v
		case "value":
			value = v
		}
	}
	flush()
	return values
}

// parseDiskStatus parses `lsdev -Cc disk -F name:status`.
func parseDiskStatus(out string) map[string]string {
	status := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		name, st, ok := strings.Cut(strings.TrimSpace(line), ":")
		if ok && aixDiskNameRe.MatchString(name) {
			status[name] = st
		}
	}
	return status
}

// countEnabledPaths counts the Enabled lines of `lspath -l <disk> -F status`.
// MPIO lists one line per path: Enabled, Disabled, Failed, Defined or Missing.
func countEnabledPaths(out string) int {
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "Enabled" {
			n++
		}
	}
	return n
}

// parseScanAdapters returns the adapters from `lsdev -Cc adapter -F name`
// that disks attach through: fcs (Fibre Channel, used for NPIV) and vscsi.
func parseScanAdapters(out string) []string {
	var adapters []string
	for _, line := range strings.Split(out, "\n") {
		name := strings.TrimSpace(line)
		if strings.HasPrefix(name, "fcs") || strings.HasPrefix(name, "vscsi") {
			adapters = append(adapters, name)
		}
	}
	return adapters
}

// uniqueIDMatchesWWN reports whether an AIX unique_id belongs to the volume
// with the given WWN. The unique_id embeds the volume's NAA identifier; for
// IBM SAN Volume Controller and FlashSystem volumes it looks like
//
//	33213 60050768108100A8B8000000000003BE 04214503IBMfcp
//
// (without the spaces), where the middle part is the WWN PowerVS reports.
func uniqueIDMatchesWWN(uniqueID, wwn string) bool {
	if len(wwn) < 16 {
		return false
	}
	return strings.Contains(strings.ToUpper(uniqueID), strings.ToUpper(wwn))
}

// wwnFromUniqueID extracts the NAA type 6 identifier (32 hex digits starting
// with 6) from an AIX unique_id, upper-cased as PowerVS reports WWNs. It
// returns "" when there is none.
func wwnFromUniqueID(uniqueID string) string {
	u := strings.ToUpper(uniqueID)
	for i := 0; i+32 <= len(u); i++ {
		if u[i] != '6' {
			continue
		}
		if isHex(u[i : i+32]) {
			return u[i : i+32]
		}
	}
	return ""
}

func isHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}
