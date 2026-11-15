package controller

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	platformv1alpha1 "github.com/mella7/project-claim/api/v1alpha1"
	"github.com/mella7/project-claim/internal/metrics"
)

// These rules live in the CRD schema, so the API server rejects the write and
// the controller never sees it. Dry-run is enough to test them.
var _ = Describe("API validation", func() {
	rejected := func(mod func(*platformv1alpha1.ProjectClaim), want string) {
		err := k8sClient.Create(ctx, claimFor("some-tenant", "some-tier", mod), client.DryRunAll)
		ExpectWithOffset(1, err).To(HaveOccurred())
		ExpectWithOffset(1, err.Error()).To(ContainSubstring(want))
	}

	DescribeTable("rejects project names that would collide with the platform",
		func(name string) {
			rejected(func(c *platformv1alpha1.ProjectClaim) { c.Spec.ProjectName = name }, "reserved name")
		},
		Entry("kube- prefix", "kube-system"),
		Entry("openshift- prefix", "openshift-monitoring"),
		Entry("default", "default"),
		Entry("openshift", "openshift"),
	)

	It("rejects a project name that is not a valid namespace name", func() {
		rejected(func(c *platformv1alpha1.ProjectClaim) { c.Spec.ProjectName = "Not_A_Namespace" }, "spec.projectName")
	})

	It("rejects extra labels that could override the operator or Pod Security", func() {
		rejected(func(c *platformv1alpha1.ProjectClaim) {
			c.Spec.ExtraLabels = map[string]string{"pod-security.kubernetes.io/enforce": "privileged"}
		}, "reserved")
	})

	It("rejects a claim name that does not fit into a label value", func() {
		rejected(func(c *platformv1alpha1.ProjectClaim) { c.Name = strings.Repeat("a", 64) }, "at most 63 characters")
	})

	It("rejects a tier without any quota", func() {
		tier := &platformv1alpha1.ProjectTier{}
		tier.Name = uniq("tier")
		tier.Spec.Quota = corev1.ResourceList{}
		err := k8sClient.Create(ctx, tier, client.DryRunAll)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("at least one resource"))
	})

	It("does not allow projectName or tenantRef to change", func() {
		tier, tenant := newTier(), newTenant()
		claim := newClaim(tenant.Name, tier.Name)

		rename := func(change func(*platformv1alpha1.ProjectClaim)) error {
			var err error
			// The operator is writing to the same claim, so an attempt can
			// lose the race and come back as a conflict. Try again then.
			Eventually(func() bool {
				fresh := getClaim(claim.Name)
				change(fresh)
				err = k8sClient.Update(ctx, fresh, client.DryRunAll)
				return apierrors.IsConflict(err)
			}).Should(BeFalse())
			return err
		}
		// A rename would leave the old namespace behind with nothing
		// pointing at it. Refusing the update is simpler than reconciling it.
		Expect(rename(func(c *platformv1alpha1.ProjectClaim) { c.Spec.ProjectName = uniq("renamed") })).
			To(MatchError(ContainSubstring("projectName is immutable")))
		Expect(rename(func(c *platformv1alpha1.ProjectClaim) { c.Spec.TenantRef.Name = "another" })).
			To(MatchError(ContainSubstring("tenantRef is immutable")))
	})
})

var _ = Describe("Metrics", func() {
	It("reports claims by phase and tenant budgets from the cache", func() {
		tier := newTier()
		tenant := newTenant(func(t *platformv1alpha1.Tenant) {
			t.Spec.QuotaBudget = corev1.ResourceList{corev1.ResourceRequestsCPU: qty("10")}
		})
		claim := newClaim(tenant.Name, tier.Name)
		waitReady(claim.Name)

		// The suite does not serve metrics, so the collector is registered
		// here. main.go does the same against the manager's cache.
		collector := metrics.NewStateCollector(k8sClient)
		Expect(ctrlmetrics.Registry.Register(collector)).To(Succeed())
		DeferCleanup(func() { ctrlmetrics.Registry.Unregister(collector) })

		value := func(family string, labels map[string]string) float64 {
			families, err := ctrlmetrics.Registry.Gather()
			Expect(err).NotTo(HaveOccurred())
			for _, f := range families {
				if f.GetName() != family {
					continue
				}
			next:
				for _, m := range f.GetMetric() {
					got := map[string]string{}
					for _, l := range m.GetLabel() {
						got[l.GetName()] = l.GetValue()
					}
					for k, v := range labels {
						if got[k] != v {
							continue next
						}
					}
					if m.GetGauge() != nil {
						return m.GetGauge().GetValue()
					}
					return m.GetCounter().GetValue()
				}
			}
			return -1
		}

		Eventually(func(g Gomega) {
			g.Expect(value("projectclaim_claims", map[string]string{"tenant": tenant.Name, "phase": "Ready"})).To(BeEquivalentTo(1))
			g.Expect(value("projectclaim_tenant_projects", map[string]string{"tenant": tenant.Name})).To(BeEquivalentTo(1))
			g.Expect(value("projectclaim_tenant_quota_allocated", map[string]string{"tenant": tenant.Name, "resource": "requests.cpu"})).To(BeEquivalentTo(2))
			g.Expect(value("projectclaim_tenant_quota_budget", map[string]string{"tenant": tenant.Name, "resource": "requests.cpu"})).To(BeEquivalentTo(10))
		}).Should(Succeed())

		Expect(value("projectclaim_apply_total", map[string]string{"kind": "Namespace", "result": "created"})).To(BeNumerically(">=", 1))
	})
})
