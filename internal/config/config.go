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

// WebhookServerConfig configures the webhook server
type WebhookServerConfig struct {
	// Host is the address that the server will listen on.
	// Defaults to "" - all addresses.
	Host string `json:"host"`

	// Port is the port number that the server will serve.
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

// MetricsServerConfig configures the metrics server
type MetricsServerConfig struct {
	// BindAddress is the TCP address that the server should bind to.
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

// DownstreamResourceManagementConfig configures how instance Pods are placed in
// the cell cluster hosting them.
//
// Instance Pods run on ordinary kubelet nodes in the same cluster as the
// provider, so ConfigMap and Secret volumes are referenced by name and resolved
// by the kubelet under its own node identity. The provider never reads or
// mirrors their contents.
type DownstreamResourceManagementConfig struct {
	// NodeSelector overrides the node selector applied to every instance Pod.
	// When unset the provider selects DefaultNodeSelector, the label
	// kata-deploy applies to every node it has installed the runtime on.
	// Override it where the runtime is installed by other means, or where a
	// subset of the Kata-capable nodes is reserved for this class.
	//
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations overrides the tolerations applied to every instance Pod.
	// Kata-capable nodes carry no taint by default, so the provider adds none;
	// set this where a deployment taints the nodes reserved for tenant
	// instances.
	//
	// +optional
	Tolerations []core.Toleration `json:"tolerations,omitempty"`

	// RuntimeHandler is the name of the Kubernetes RuntimeClass the instance
	// Pods run under. Which Kata hypervisor a site runs is a deployment
	// decision — kata-qemu and kata-clh are installed under different handler
	// names and differ in device support and startup latency — so it is
	// configured rather than compiled in. Defaults to DefaultRuntimeHandler.
	//
	// A tenant cannot influence this value: it comes from provider
	// configuration, never from the Instance.
	//
	// +optional
	// +default="kata-qemu"
	RuntimeHandler string `json:"runtimeHandler,omitempty"`
}
