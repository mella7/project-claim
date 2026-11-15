// Package metrics defines the operator's own Prometheus metrics. They are
// registered next to the ones controller-runtime exports by default
// (reconcile counts, durations, queue depth).
package metrics

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	platformv1alpha1 "github.com/mella7/project-claim/api/v1alpha1"
)

// ApplyTotal counts what happened each time the operator compared a managed
// object with its desired state. In a settled cluster only result="unchanged"
// grows. Growth in "updated" without anyone editing claims means something
// keeps changing the operator's objects.
var ApplyTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "projectclaim_apply_total",
	Help: "Outcome of applying a managed object, by kind and result (created, updated, unchanged, deleted, error).",
}, []string{"kind", "result"})

func init() {
	ctrlmetrics.Registry.MustRegister(ApplyTotal)
}

var (
	claimsDesc = prometheus.NewDesc("projectclaim_claims",
		"Number of ProjectClaims by tenant, tier and phase.", []string{"tenant", "tier", "phase"}, nil)
	tenantProjectsDesc = prometheus.NewDesc("projectclaim_tenant_projects",
		"Number of projects holding a reservation, by tenant.", []string{"tenant"}, nil)
	tenantAllocatedDesc = prometheus.NewDesc("projectclaim_tenant_quota_allocated",
		"Quota reserved by the tenant's projects, in base units (cores, bytes, count).", []string{"tenant", "resource"}, nil)
	tenantBudgetDesc = prometheus.NewDesc("projectclaim_tenant_quota_budget",
		"Quota budget of the tenant, in base units (cores, bytes, count).", []string{"tenant", "resource"}, nil)
)

// StateCollector reports the state of claims and tenants. It reads the
// informer cache when Prometheus scrapes, so the numbers cannot drift from
// reality the way hand-maintained gauges do, and scraping costs the API
// server nothing.
type StateCollector struct {
	reader client.Reader
}

// NewStateCollector returns a collector that lists from the given reader,
// normally the manager's cache.
func NewStateCollector(reader client.Reader) *StateCollector {
	return &StateCollector{reader: reader}
}

func (c *StateCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- claimsDesc
	ch <- tenantProjectsDesc
	ch <- tenantAllocatedDesc
	ch <- tenantBudgetDesc
}

func (c *StateCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	claims := &platformv1alpha1.ProjectClaimList{}
	if err := c.reader.List(ctx, claims); err == nil {
		type key struct{ tenant, tier, phase string }
		counts := map[key]int{}
		for i := range claims.Items {
			claim := &claims.Items[i]
			phase := string(claim.Status.Phase)
			if phase == "" {
				phase = string(platformv1alpha1.PhasePending)
			}
			counts[key{claim.Spec.TenantRef.Name, claim.Spec.Tier, phase}]++
		}
		for k, n := range counts {
			ch <- prometheus.MustNewConstMetric(claimsDesc, prometheus.GaugeValue, float64(n), k.tenant, k.tier, k.phase)
		}
	}

	tenants := &platformv1alpha1.TenantList{}
	if err := c.reader.List(ctx, tenants); err == nil {
		for i := range tenants.Items {
			tenant := &tenants.Items[i]
			ch <- prometheus.MustNewConstMetric(tenantProjectsDesc, prometheus.GaugeValue,
				float64(tenant.Status.ProjectCount), tenant.Name)
			for name, q := range tenant.Status.Allocated {
				ch <- prometheus.MustNewConstMetric(tenantAllocatedDesc, prometheus.GaugeValue,
					q.AsApproximateFloat64(), tenant.Name, string(name))
			}
			for name, q := range tenant.Spec.QuotaBudget {
				ch <- prometheus.MustNewConstMetric(tenantBudgetDesc, prometheus.GaugeValue,
					q.AsApproximateFloat64(), tenant.Name, string(name))
			}
		}
	}
}
