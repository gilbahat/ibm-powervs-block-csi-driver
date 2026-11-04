/*
Copyright 2020 The Kubernetes Authors.

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

package options

// DONE.

import (
	"flag"

	"sigs.k8s.io/ibm-powervs-block-csi-driver/pkg/driver"
)

// NodeOptions contains options and configuration settings for the node service.
type NodeOptions struct {
	VolumeAttachLimit int64

	// KubeletRegistrationPath and PluginRegistrationDir are for a node
	// plugin that runs on the host (AIX) and registers itself with the
	// kubelet; in a pod, node-driver-registrar does this.
	KubeletRegistrationPath string
	PluginRegistrationDir   string
}

func (o *NodeOptions) AddFlags(fs *flag.FlagSet) {
	fs.StringVar(&o.KubeletRegistrationPath, "kubelet-registration-path", "", "Register the node plugin with the kubelet, advertising this CSI socket path. For node plugins run outside a pod (AIX); leave unset when node-driver-registrar runs alongside.")
	fs.StringVar(&o.PluginRegistrationDir, "plugin-registration-dir", driver.DefaultPluginRegistrationDir, "The kubelet's plugin registration directory, used with --kubelet-registration-path.")
	fs.Int64Var(&o.VolumeAttachLimit, "volume-attach-limit", -1, "Value for the maximum number of volumes attachable per node. If specified, the limit applies to all nodes. If not specified, the value is approximated from the instance type.")
}
