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

package driver

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"google.golang.org/grpc"
	"k8s.io/klog/v2"
	registerapi "k8s.io/kubelet/pkg/apis/pluginregistration/v1"
)

// DefaultPluginRegistrationDir is the kubelet's plugin registration directory.
const DefaultPluginRegistrationDir = "/var/lib/kubelet/plugins_registry"

// kubeletRegistration does what node-driver-registrar does, inside the
// plugin: it serves the kubelet's plugin registration API on a socket in the
// kubelet's registration directory, which the kubelet's plugin watcher finds
// and calls back on.
//
// It is for nodes where the plugin runs on the host rather than in a pod,
// and so has no sidecar: on AIX the node plugin is an SRC subsystem, since
// AIX has no mount propagation. It is off unless
// --kubelet-registration-path is set; the Linux DaemonSet keeps using
// node-driver-registrar.
//
// Adapted from github.com/gilbahat/csi-driver-nfs pkg/nfs/registration.go.
type kubeletRegistration struct {
	registerapi.UnimplementedRegistrationServer

	driverName string
	// endpoint is the CSI socket path the kubelet should dial.
	endpoint string
	// onFailure is called when the kubelet reports that registration failed.
	onFailure func(msg string)
}

// registrationSocketPath is where node-driver-registrar would put the socket.
func registrationSocketPath(dir, driverName string) string {
	return filepath.Join(dir, driverName+"-reg.sock")
}

// GetInfo tells the kubelet which CSI driver this is and where it listens.
func (r *kubeletRegistration) GetInfo(_ context.Context, _ *registerapi.InfoRequest) (*registerapi.PluginInfo, error) {
	klog.V(2).Infof("kubelet registration: GetInfo for %s", r.driverName)
	return &registerapi.PluginInfo{
		Type:              registerapi.CSIPlugin,
		Name:              r.driverName,
		Endpoint:          r.endpoint,
		SupportedVersions: []string{"1.0.0"},
	}, nil
}

// NotifyRegistrationStatus receives the kubelet's verdict. A failure is
// fatal, as in node-driver-registrar, so the service manager restarts the
// plugin and registration is tried again.
func (r *kubeletRegistration) NotifyRegistrationStatus(_ context.Context, status *registerapi.RegistrationStatus) (*registerapi.RegistrationStatusResponse, error) {
	if !status.PluginRegistered {
		msg := fmt.Sprintf("kubelet failed to register %s: %s", r.driverName, status.Error)
		klog.Error(msg)
		if r.onFailure != nil {
			r.onFailure(msg)
		}
		return &registerapi.RegistrationStatusResponse{}, nil
	}
	klog.Infof("kubelet registered %s (endpoint %s)", r.driverName, r.endpoint)
	return &registerapi.RegistrationStatusResponse{}, nil
}

// serveKubeletRegistration starts the registration server and returns a
// function that stops it and removes its socket.
func serveKubeletRegistration(dir string, reg *kubeletRegistration) (func(), error) {
	if reg.driverName == "" || reg.endpoint == "" {
		return nil, fmt.Errorf("driver name and endpoint are required for kubelet registration")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("creating registration directory: %w", err)
	}
	sock := registrationSocketPath(dir, reg.driverName)
	if err := os.Remove(sock); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("removing stale registration socket %s: %w", sock, err)
	}
	lis, err := net.Listen("unix", sock)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", sock, err)
	}
	server := grpc.NewServer()
	registerapi.RegisterRegistrationServer(server, reg)
	go func() {
		klog.Infof("kubelet registration: serving %s", sock)
		if err := server.Serve(lis); err != nil {
			klog.Errorf("kubelet registration server stopped: %v", err)
		}
	}()
	return func() {
		server.Stop()
		_ = os.Remove(sock)
	}, nil
}

// startKubeletRegistration registers the node plugin with the kubelet when
// --kubelet-registration-path is set. It runs after the CSI listener exists,
// so the kubelet's first call (NodeGetInfo) is queued rather than refused.
// On SIGTERM (stopsrc) it removes both sockets: the kubelet unregisters a
// plugin only when its registration socket disappears.
func (d *Driver) startKubeletRegistration(csiSocket string) error {
	o := d.options
	if o.kubeletRegistrationPath == "" || o.mode == ControllerMode {
		return nil
	}
	dir := o.pluginRegistrationDir
	if dir == "" {
		dir = DefaultPluginRegistrationDir
	}
	reg := &kubeletRegistration{
		driverName: DriverName,
		endpoint:   o.kubeletRegistrationPath,
		// Exit so the service manager restarts the plugin, as the kubelet
		// restarts a failed node-driver-registrar container.
		onFailure: func(string) { os.Exit(1) },
	}
	stop, err := serveKubeletRegistration(dir, reg)
	if err != nil {
		return err
	}
	go func() {
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer cancel()
		<-ctx.Done()
		klog.Info("stopping: removing kubelet registration and CSI sockets")
		stop()
		if csiSocket != "" {
			_ = os.Remove(csiSocket)
		}
		klog.Flush()
		os.Exit(0)
	}()
	return nil
}
