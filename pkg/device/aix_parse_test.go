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

package device

import (
	"reflect"
	"testing"
)

const odmUniqueIDs = `
CuAt:
        name = "hdisk0"
        attribute = "unique_id"
        value = "3321360050768108100A8B8000000000000A104214503IBMfcp"
        type = "R"
        generic = ""
        rep = "nl"
        nls_index = 79

CuAt:
        name = "hdisk4"
        attribute = "unique_id"
        value = "3321360050768108100A8B8000000000003BE04214503IBMfcp"
        type = "R"
        generic = ""
        rep = "nl"
        nls_index = 79
`

func TestParseODMAttr(t *testing.T) {
	got := parseODMAttr(odmUniqueIDs)
	want := map[string]string{
		"hdisk0": "3321360050768108100A8B8000000000000A104214503IBMfcp",
		"hdisk4": "3321360050768108100A8B8000000000003BE04214503IBMfcp",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}
}

func TestWWNFromUniqueID(t *testing.T) {
	for uid, want := range map[string]string{
		"3321360050768108100A8B8000000000003BE04214503IBMfcp": "60050768108100A8B8000000000003BE",
		"33213600507680c8080b3d800000000000c9204214503IBMfcp": "600507680C8080B3D800000000000C92",
		"no identifier here": "",
	} {
		if got := wwnFromUniqueID(uid); got != want {
			t.Errorf("wwnFromUniqueID(%q) = %q, want %q", uid, got, want)
		}
	}
}

func TestUniqueIDMatchesWWN(t *testing.T) {
	uid := "3321360050768108100A8B8000000000003BE04214503IBMfcp"
	if !uniqueIDMatchesWWN(uid, "60050768108100a8b8000000000003be") {
		t.Error("lower-case WWN did not match")
	}
	if uniqueIDMatchesWWN(uid, "60050768108100A8B8000000000003BF") {
		t.Error("other WWN matched")
	}
	if uniqueIDMatchesWWN(uid, "") || uniqueIDMatchesWWN(uid, "3BE") {
		t.Error("short WWN matched")
	}
}

func TestParseDiskStatus(t *testing.T) {
	got := parseDiskStatus("hdisk0:Available\nhdisk4:Defined\ncd0:Available\n\n")
	want := map[string]string{"hdisk0": "Available", "hdisk4": "Defined"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}
}

func TestCountEnabledPaths(t *testing.T) {
	if n := countEnabledPaths("Enabled\nEnabled\nFailed\nMissing\nEnabled\n"); n != 3 {
		t.Errorf("got %d", n)
	}
	if n := countEnabledPaths("Defined\nDefined\n"); n != 0 {
		t.Errorf("got %d", n)
	}
}

func TestParseScanAdapters(t *testing.T) {
	got := parseScanAdapters("ent0\nfcs0\nfcs1\nvscsi0\nsissas0\n")
	want := []string{"fcs0", "fcs1", "vscsi0"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}
}
