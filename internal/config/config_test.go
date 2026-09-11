// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"os"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
)

// defaultRuntimeHandler is the handler the shipped configuration and the type
// defaults both name.
const defaultRuntimeHandler = "kata-clh"

// TestDecodeShippedConfig decodes the config file the deployment mounts, with
// the same strict decoder the manager uses. A setting that the shipped config
// names but the type no longer has would otherwise only surface as a manager
// that will not start.
func TestDecodeShippedConfig(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add config scheme: %v", err)
	}
	if err := RegisterDefaults(scheme); err != nil {
		t.Fatalf("failed to register defaults: %v", err)
	}
	codecs := serializer.NewCodecFactory(scheme, serializer.EnableStrict)

	data, err := os.ReadFile("../../config/base/manager/config.yaml")
	if err != nil {
		t.Fatalf("failed to read the shipped config: %v", err)
	}

	var config KataProvider
	if err := runtime.DecodeInto(codecs.UniversalDecoder(), data, &config); err != nil {
		t.Fatalf("failed to decode the shipped config: %v", err)
	}

	if got := config.DownstreamResourceManagement.RuntimeHandler; got != defaultRuntimeHandler {
		t.Errorf("runtimeHandler = %q, want %q", got, defaultRuntimeHandler)
	}
}

// TestExplicitRuntimeHandlerSurvivesDefaulting decodes a config that names
// kata-qemu. Defaulting must leave it alone: an arm64 site has no Cloud
// Hypervisor shim to run, so overriding the handler is the only way it can
// serve the tier at all.
func TestExplicitRuntimeHandlerSurvivesDefaulting(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add config scheme: %v", err)
	}
	if err := RegisterDefaults(scheme); err != nil {
		t.Fatalf("failed to register defaults: %v", err)
	}
	codecs := serializer.NewCodecFactory(scheme, serializer.EnableStrict)

	data := []byte(`apiVersion: apiserver.config.datumapis.com/v1alpha1
kind: KataProvider
downstreamResourceManagement:
  runtimeHandler: kata-qemu
`)

	var config KataProvider
	if err := runtime.DecodeInto(codecs.UniversalDecoder(), data, &config); err != nil {
		t.Fatalf("failed to decode the config: %v", err)
	}

	if got := config.DownstreamResourceManagement.RuntimeHandler; got != "kata-qemu" {
		t.Errorf("runtimeHandler = %q, want %q", got, "kata-qemu")
	}
}

// TestRuntimeHandlerIsOpaque decodes a config naming the Rust runtime-rs shim,
// which no RuntimeClass in this repository declares by default. The provider
// compiles in no list of known handlers, so a cell selects a shim by
// configuration alone.
func TestRuntimeHandlerIsOpaque(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add config scheme: %v", err)
	}
	if err := RegisterDefaults(scheme); err != nil {
		t.Fatalf("failed to register defaults: %v", err)
	}
	codecs := serializer.NewCodecFactory(scheme, serializer.EnableStrict)

	data := []byte(`apiVersion: apiserver.config.datumapis.com/v1alpha1
kind: KataProvider
downstreamResourceManagement:
  runtimeHandler: katars
`)

	var config KataProvider
	if err := runtime.DecodeInto(codecs.UniversalDecoder(), data, &config); err != nil {
		t.Fatalf("failed to decode the config: %v", err)
	}

	if got := config.DownstreamResourceManagement.RuntimeHandler; got != "katars" {
		t.Errorf("runtimeHandler = %q, want %q", got, "katars")
	}
}

// TestVPCNetworkingIsOffByDefault checks the setting that puts an instance on
// its tenant network. A cell without the VPC controller cannot serve the
// request, so reaching a tenant network is something a cell opts into.
func TestVPCNetworkingIsOffByDefault(t *testing.T) {
	var config KataProvider
	SetObjectDefaults_KataProvider(&config)

	if config.DownstreamResourceManagement.EnableVPCNetworking {
		t.Error("expected tenant networking to be opt-in")
	}
}

// TestDefaults checks that a deployment supplying no config file still runs
// against a named Kata handler rather than the cluster's default runtime, which
// would silently give tenants a shared kernel.
func TestDefaults(t *testing.T) {
	var config KataProvider
	SetObjectDefaults_KataProvider(&config)

	if got := config.DownstreamResourceManagement.RuntimeHandler; got != defaultRuntimeHandler {
		t.Errorf("runtimeHandler = %q, want %q", got, defaultRuntimeHandler)
	}
	if got := config.MetricsServer.BindAddress; got == "" {
		t.Error("expected a default metrics bind address")
	}
}
