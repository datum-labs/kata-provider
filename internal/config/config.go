// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"context"

	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:defaulter-gen=true

// KataProvider defines the configuration for the provider serving the
// general-purpose runtime class.
type KataProvider struct {
	metav1.TypeMeta

	MetricsServer MetricsServerConfig `json:"metricsServer"`

	WebhookServer WebhookServerConfig `json:"webhookServer"`

	DownstreamResourceManagement DownstreamResourceManagementConfig `json:"downstreamResourceManagement"`
}

// +k8s:deepcopy-gen=true

// WebhookServerConfig configures the webhook server.
type WebhookServerConfig struct {
	// Host is the address that the server listens on. An empty value means all
	// addresses.
	Host string `json:"host"`

	// Port is the port number that the server listens on.
	// +default=9443
	Port int `json:"port"`

	// CertDir is the directory that contains the server key and certificate.
	CertDir string `json:"certDir"`

	// CertName is the server certificate name. Defaults to tls.crt.
	CertName string `json:"certName"`

	// KeyName is the server key name. Defaults to tls.key.
	KeyName string `json:"keyName"`
}

func (w *WebhookServerConfig) Options(_ context.Context, _ client.Client) webhook.Options {
	return webhook.Options{
		Host:     w.Host,
		Port:     w.Port,
		CertDir:  w.CertDir,
		CertName: w.CertName,
		KeyName:  w.KeyName,
	}
}

// +k8s:deepcopy-gen=true

// MetricsServerConfig configures the metrics server.
type MetricsServerConfig struct {
	// BindAddress is the TCP address that the server binds to.
	// +default=":8080"
	BindAddress string `json:"bindAddress"`

	// SecureServing configures the secure serving options.
	SecureServing bool `json:"secureServing"`

	// CertDir is the directory that contains the server key and certificate.
	CertDir string `json:"certDir"`

	// CertName is the server certificate name. Defaults to tls.crt.
	CertName string `json:"certName"`

	// KeyName is the server key name. Defaults to tls.key.
	KeyName string `json:"keyName"`
}

func (m *MetricsServerConfig) Options(_ context.Context, _ client.Client) metricsserver.Options {
	opts := metricsserver.Options{
		BindAddress:   m.BindAddress,
		SecureServing: m.SecureServing,
		CertDir:       m.CertDir,
		CertName:      m.CertName,
		KeyName:       m.KeyName,
	}

	if m.SecureServing {
		opts.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	return opts
}

// +k8s:deepcopy-gen=true

// DownstreamResourceManagementConfig configures how the provider places
// instance Pods in the cell cluster that hosts them.
//
// Instance Pods run on ordinary kubelet nodes in the same cluster as the
// provider. A Pod therefore refers to ConfigMap and Secret volumes by name, and
// the kubelet resolves them under its own node identity. The provider never
// reads or mirrors their contents.
type DownstreamResourceManagementConfig struct {
	// NodeSelector overrides the node selector that the provider applies to
	// every instance Pod. When the field is unset, the provider selects
	// DefaultNodeSelector, which is the label that kata-deploy applies to every
	// node where it installed the runtime. Override the field where another
	// mechanism installs the runtime, or where a deployment reserves a subset
	// of the Kata-capable nodes for this class.
	//
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations overrides the tolerations that the provider applies to every
	// instance Pod. Kata-capable nodes carry no taint by default, so the
	// provider adds none. Set this field where a deployment taints the nodes
	// reserved for tenant instances.
	//
	// +optional
	Tolerations []core.Toleration `json:"tolerations,omitempty"`

	// RuntimeHandler is the name of the Kubernetes RuntimeClass that the
	// instance Pods run under. The Kata hypervisor that a site runs is a
	// deployment decision, so this field is configurable rather than compiled
	// in. kata-qemu and kata-clh install under different handler names, and
	// they differ in device support and in startup latency. Defaults to
	// DefaultRuntimeHandler.
	//
	// A tenant cannot influence this value. The value comes from provider
	// configuration, never from the Instance.
	//
	// +optional
	// +default="kata-qemu"
	RuntimeHandler string `json:"runtimeHandler,omitempty"`
}
