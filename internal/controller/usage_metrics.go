// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Usage metering counters, served on the manager's metrics endpoint. Every
// path that deliberately does not bill running time increments
// usageWindowsDropped, so an alert on it catches attribution or delivery
// problems before they show up as missing revenue.
var (
	usageWindowsEmitted = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kata_provider_usage_windows_emitted_total",
		Help: "Usage windows delivered to the billing collector (one CPU and one memory event each).",
	})

	usageWindowsDropped = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kata_provider_usage_windows_dropped_total",
		Help: "Usage windows deliberately not billed, by reason (no_project, no_resources, invalid, rejected, emit_give_up).",
	}, []string{"reason"})

	usageEmitErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kata_provider_usage_emit_errors_total",
		Help: "Failed attempts to deliver a usage window to the billing collector. Failed windows are retried.",
	})

	usageBackfillCapped = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kata_provider_usage_backfill_capped_total",
		Help: "Pods first observed long after they started, whose first usage window was capped at the backfill limit.",
	})

	usageForcedClose = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kata_provider_usage_forced_close_total",
		Help: "Deleted Pods whose containers were never reported terminated, closed at their DeletionTimestamp.",
	})
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		usageWindowsEmitted,
		usageWindowsDropped,
		usageEmitErrors,
		usageBackfillCapped,
		usageForcedClose,
	)
}
