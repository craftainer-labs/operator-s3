package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// reconcileTotal counts reconciles of the Greeting kind by outcome. It is
// registered on controller-runtime's registry, so it is served on the same
// authenticated /metrics endpoint as the built-in controller metrics.
var reconcileTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "greeting_reconcile_total",
		Help: "Reconciles of Greeting objects by result (success or error).",
	},
	[]string{"result"},
)

func init() {
	metrics.Registry.MustRegister(reconcileTotal)
}

// countReconcile records one reconcile; a non-nil err counts as an error.
func countReconcile(err error) {
	result := "success"
	if err != nil {
		result = "error"
	}
	reconcileTotal.WithLabelValues(result).Inc()
}
