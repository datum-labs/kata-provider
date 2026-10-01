// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"os"
	"reflect"
	"testing"

	core "k8s.io/api/core/v1"

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

// TestInstanceDNSDecodesIPv6OnlyNameservers decodes the resolver configuration
// a production cell sets: public IPv6 resolvers and nothing else. The field
// mirrors a Pod's dnsConfig, so an IPv6-only list must decode and validate
// unchanged.
func TestInstanceDNSDecodesIPv6OnlyNameservers(t *testing.T) {
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
  instanceDNS:
    nameservers:
      - 2606:4700:4700::1111
      - 2001:4860:4860::8888
    searches:
      - example.internal
    options:
      - name: ndots
        value: "1"
`)

	var config KataProvider
	if err := runtime.DecodeInto(codecs.UniversalDecoder(), data, &config); err != nil {
		t.Fatalf("failed to decode the config: %v", err)
	}
	if err := config.DownstreamResourceManagement.Validate(); err != nil {
		t.Fatalf("expected an IPv6-only nameserver list to validate: %v", err)
	}

	dns := config.DownstreamResourceManagement.InstanceDNS
	if dns == nil {
		t.Fatal("expected instanceDNS to decode")
	}
	wantNameservers := []string{"2606:4700:4700::1111", "2001:4860:4860::8888"}
	if got := dns.Nameservers; !reflect.DeepEqual(got, wantNameservers) {
		t.Errorf("nameservers = %v, want %v", got, wantNameservers)
	}
	if got := dns.Searches; !reflect.DeepEqual(got, []string{"example.internal"}) {
		t.Errorf("searches = %v, want [example.internal]", got)
	}
	if len(dns.Options) != 1 || dns.Options[0].Name != "ndots" || dns.Options[0].Value == nil || *dns.Options[0].Value != "1" {
		t.Errorf("options = %+v, want ndots=1", dns.Options)
	}
}

// TestInstanceDNSIsUnsetByDefault checks that a deployment supplying no
// resolver configuration leaves instances on the cluster default. Defaulting
// must not invent a resolver.
func TestInstanceDNSIsUnsetByDefault(t *testing.T) {
	var config KataProvider
	SetObjectDefaults_KataProvider(&config)

	if config.DownstreamResourceManagement.InstanceDNS != nil {
		t.Error("expected instanceDNS to be unset by default")
	}
	if err := config.DownstreamResourceManagement.Validate(); err != nil {
		t.Errorf("expected an unset instanceDNS to validate: %v", err)
	}
}

// TestInstanceDNSValidation covers the configurations the API server would
// refuse on every instance Pod. The provider reports them at startup instead.
func TestInstanceDNSValidation(t *testing.T) {
	tests := []struct {
		name    string
		dns     *core.PodDNSConfig
		wantErr bool
	}{
		{name: "no nameservers", dns: &core.PodDNSConfig{Searches: []string{"example.internal"}}, wantErr: true},
		{name: "hostname nameserver", dns: &core.PodDNSConfig{Nameservers: []string{"dns.google"}}, wantErr: true},
		{name: "IPv4 nameserver", dns: &core.PodDNSConfig{Nameservers: []string{"1.1.1.1"}}},
		{name: "IPv6 nameserver", dns: &core.PodDNSConfig{Nameservers: []string{"2606:4700:4700::1111"}}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			config := DownstreamResourceManagementConfig{InstanceDNS: tc.dns}
			err := config.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
