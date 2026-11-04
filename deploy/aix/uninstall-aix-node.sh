#!/usr/bin/ksh
# Copyright 2026 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Removes the PowerVS CSI node plugin subsystem installed by
# install-aix-node.sh. Volumes staged on this node stay mounted and their
# volume groups stay imported: drain the node first.
#
# Settings: SUBSYS (csi-powervs), BIN (to find the wrapper), KUBELET_DIR,
# KEEP_CONFIG=1 to keep /etc/ibm-powervs-csi (it holds the API key).

BIN=${BIN:-/opt/ibm-powervs-block-csi-driver/bin/ibm-powervs-block-csi-driver}
SUBSYS=${SUBSYS:-csi-powervs}
KUBELET_DIR=${KUBELET_DIR:-/var/lib/kubelet}
ITAB_ID=$(echo "${SUBSYS}" | tr -cd '[:alnum:]' | cut -c1-14)

if lssrc -s "${SUBSYS}" >/dev/null 2>&1; then
	stopsrc -s "${SUBSYS}" >/dev/null 2>&1
	sleep 5
	rmssys -s "${SUBSYS}"
fi
rmitab "${ITAB_ID}" >/dev/null 2>&1

# stopsrc's SIGTERM removes these; this covers a plugin that was killed.
rm -f "${KUBELET_DIR}/plugins_registry/powervs.csi.ibm.com-reg.sock" \
	"${KUBELET_DIR}/plugins/powervs.csi.ibm.com/csi.sock"
rm -f "$(dirname "${BIN}")/powervs-csi-node"
[ "${KEEP_CONFIG}" = "1" ] || rm -rf /etc/ibm-powervs-csi

echo "${SUBSYS} removed"
