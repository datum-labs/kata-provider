// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"os"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
)

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

	if got := config.DownstreamResourceManagement.RuntimeHandler; got != "kata-qemu" {
		t.Errorf("runtimeHandler = %q, want %q", got, "kata-qemu")
	}
}

// TestDefaults checks that a deployment supplying no config file still runs
// against a named Kata handler rather than the cluster's default runtime, which
// would silently give tenants a shared kernel.
func TestDefaults(t *testing.T) {
	var config KataProvider
	SetObjectDefaults_KataProvider(&config)

	if got := config.DownstreamResourceManagement.RuntimeHandler; got == "" {
		t.Error("expected a default Kubernetes RuntimeClass handler")
	}
	if got := config.MetricsServer.BindAddress; got == "" {
		t.Error("expected a default metrics bind address")
	}
}
