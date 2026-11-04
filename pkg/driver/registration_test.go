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
	"os"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	registerapi "k8s.io/kubelet/pkg/apis/pluginregistration/v1"
)

func TestKubeletRegistration(t *testing.T) {
	// Unix socket paths are length-limited; keep the directory short.
	dir, err := os.MkdirTemp("", "reg")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	// A stale socket file from a previous run must not block startup.
	sock := registrationSocketPath(dir, DriverName)
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	failed := make(chan string, 1)
	reg := &kubeletRegistration{
		driverName: DriverName,
		endpoint:   "/var/lib/kubelet/plugins/powervs.csi.ibm.com/csi.sock",
		onFailure:  func(msg string) { failed <- msg },
	}
	stop, err := serveKubeletRegistration(dir, reg)
	if err != nil {
		t.Fatalf("serveKubeletRegistration: %v", err)
	}
	defer stop()

	conn, err := grpc.NewClient("unix://"+sock, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := registerapi.NewRegistrationClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	info, err := client.GetInfo(ctx, &registerapi.InfoRequest{})
	if err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	if info.Type != registerapi.CSIPlugin || info.Name != DriverName || info.Endpoint != reg.endpoint {
		t.Errorf("GetInfo = %+v", info)
	}
	if len(info.SupportedVersions) != 1 || info.SupportedVersions[0] != "1.0.0" {
		t.Errorf("SupportedVersions = %v", info.SupportedVersions)
	}

	if _, err := client.NotifyRegistrationStatus(ctx, &registerapi.RegistrationStatus{PluginRegistered: true}); err != nil {
		t.Fatalf("NotifyRegistrationStatus(ok): %v", err)
	}
	select {
	case msg := <-failed:
		t.Fatalf("onFailure called on success: %s", msg)
	default:
	}

	if _, err := client.NotifyRegistrationStatus(ctx, &registerapi.RegistrationStatus{PluginRegistered: false, Error: "boom"}); err != nil {
		t.Fatalf("NotifyRegistrationStatus(fail): %v", err)
	}
	select {
	case <-failed:
	case <-time.After(time.Second):
		t.Fatal("onFailure not called on registration failure")
	}
}

func TestServeKubeletRegistrationNeedsEndpoint(t *testing.T) {
	if _, err := serveKubeletRegistration(t.TempDir(), &kubeletRegistration{driverName: DriverName}); err == nil {
		t.Fatal("expected an error without an endpoint")
	}
}

func TestStartKubeletRegistrationOffByDefault(t *testing.T) {
	d := &Driver{options: &Options{mode: NodeMode}}
	if err := d.startKubeletRegistration(""); err != nil {
		t.Fatal(err)
	}
	d = &Driver{options: &Options{mode: ControllerMode, kubeletRegistrationPath: "/x/csi.sock", pluginRegistrationDir: "/nonexistent/should-not-be-created"}}
	if err := d.startKubeletRegistration(""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat("/nonexistent/should-not-be-created"); err == nil {
		t.Error("controller mode created a registration directory")
	}
}
