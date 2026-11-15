package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	platformv1alpha1 "github.com/mella7/project-claim/api/v1alpha1"
)

var _ = Describe("Adoption guard", func() {
	It("refuses a namespace it did not create and leaves it untouched", func() {
		tier, tenant := newTier(), newTenant()

		foreign := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:   uniq("existing"),
			Labels: map[string]string{"important": "yes"},
		}}
		Expect(k8sClient.Create(ctx, foreign)).To(Succeed())

		claim := newClaim(tenant.Name, tier.Name, func(c *platformv1alpha1.ProjectClaim) {
			c.Spec.ProjectName = foreign.Name
		})
		waitRefused(claim.Name, platformv1alpha1.PhaseFailed, ReasonNamespaceConflict)

		// This is the check that keeps a claim for "kube-system" from taking
		// over kube-system.
		Consistently(func(g Gomega) {
			ns := &corev1.Namespace{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(foreign), ns)).To(Succeed())
			g.Expect(metav1.GetControllerOf(ns)).To(BeNil())
			g.Expect(ns.Labels).NotTo(HaveKey(LabelManagedBy))
			g.Expect(ns.Labels).To(HaveKeyWithValue("important", "yes"))
			g.Expect(exists(&corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Namespace: foreign.Name, Name: QuotaName}})).To(BeFalse())
		}).Should(Succeed())

		By("holding no budget for a claim that can never be served")
		Expect(reservationOf(getTenant(tenant.Name), claim.Name)).To(BeNil())

		By("letting the claim go without deleting the namespace")
		Expect(k8sClient.Delete(ctx, claim)).To(Succeed())
		Eventually(func() bool {
			return exists(&platformv1alpha1.ProjectClaim{ObjectMeta: metav1.ObjectMeta{Name: claim.Name}})
		}).Should(BeFalse())

		ns := &corev1.Namespace{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(foreign), ns)).To(Succeed())
		Expect(ns.DeletionTimestamp.IsZero()).To(BeTrue())
	})

	It("adopts an existing namespace that an admin handed over", func() {
		tier, tenant := newTier(), newTenant()
		claim := claimFor(tenant.Name, tier.Name)

		existing := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:        claim.Spec.ProjectName,
			Annotations: map[string]string{AnnotationAdoptInto: claim.Name},
		}}
		Expect(k8sClient.Create(ctx, existing)).To(Succeed())

		// A quota of the operator's name that the operator did not label. It
		// is outside the label-filtered cache, so this also covers the path
		// where the operator has to read from the API server to take over.
		stale := &corev1.ResourceQuota{
			ObjectMeta: metav1.ObjectMeta{Namespace: existing.Name, Name: QuotaName},
			Spec:       corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{corev1.ResourcePods: qty("1")}},
		}
		Expect(k8sClient.Create(ctx, stale)).To(Succeed())

		Expect(k8sClient.Create(ctx, claim)).To(Succeed())
		waitReady(claim.Name)

		ns := &corev1.Namespace{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(existing), ns)).To(Succeed())
		Expect(metav1.GetControllerOf(ns)).NotTo(BeNil())
		Expect(ns.UID).To(Equal(existing.UID), "the namespace is adopted, not replaced")

		quota := &corev1.ResourceQuota{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(stale), quota)).To(Succeed())
		pods := quota.Spec.Hard[corev1.ResourcePods]
		Expect(pods.Cmp(qty("20"))).To(BeZero())
		Expect(quota.Labels).To(HaveKeyWithValue(LabelClaim, claim.Name))
	})
})

var _ = Describe("Tenant policy", func() {
	It("holds a claim until its tenant exists", func() {
		tier := newTier()
		tenantName := uniq("tenant")
		claim := newClaim(tenantName, tier.Name)
		waitRefused(claim.Name, platformv1alpha1.PhasePending, ReasonTenantNotFound)

		newTenant(func(t *platformv1alpha1.Tenant) { t.Name = tenantName })
		waitReady(claim.Name)
	})

	It("only admits tiers the tenant is allowed to use", func() {
		allowed, other := newTier(), newTier()
		tenant := newTenant(func(t *platformv1alpha1.Tenant) { t.Spec.AllowedTiers = []string{allowed.Name} })

		refused := newClaim(tenant.Name, other.Name)
		waitRefused(refused.Name, platformv1alpha1.PhasePending, ReasonTierNotAllowed)

		ok := newClaim(tenant.Name, allowed.Name)
		waitReady(ok.Name)
	})

	It("keeps a tenant inside its namespace prefix", func() {
		tier := newTier()
		tenant := newTenant(func(t *platformv1alpha1.Tenant) { t.Spec.NamespacePrefix = "payments-" })

		outside := newClaim(tenant.Name, tier.Name)
		waitRefused(outside.Name, platformv1alpha1.PhasePending, ReasonNamespacePrefixMismatch)

		inside := newClaim(tenant.Name, tier.Name, func(c *platformv1alpha1.ProjectClaim) {
			c.Spec.ProjectName = uniq("payments")
		})
		waitReady(inside.Name)
	})

	It("caps the number of projects and admits the next one when a slot frees up", func() {
		tier := newTier()
		one := int32(1)
		tenant := newTenant(func(t *platformv1alpha1.Tenant) { t.Spec.MaxProjects = &one })

		first := newClaim(tenant.Name, tier.Name, func(c *platformv1alpha1.ProjectClaim) {
			c.Spec.ReclaimPolicy = platformv1alpha1.ReclaimRetain
		})
		waitReady(first.Name)

		second := newClaim(tenant.Name, tier.Name)
		waitRefused(second.Name, platformv1alpha1.PhasePending, ReasonProjectLimitReached)

		// Nobody touches the second claim. Releasing the reservation changes
		// the tenant's status, and the watch on Tenant wakes what is waiting.
		Expect(k8sClient.Delete(ctx, first)).To(Succeed())
		waitReady(second.Name)
	})
})

var _ = Describe("Tenant budget", func() {
	budget := func(cpu string) func(*platformv1alpha1.Tenant) {
		return func(t *platformv1alpha1.Tenant) {
			t.Spec.QuotaBudget = corev1.ResourceList{corev1.ResourceRequestsCPU: qty(cpu)}
		}
	}

	It("refuses a claim that does not fit and admits it once the budget grows", func() {
		tier := newTier() // 2 CPU per project
		tenant := newTenant(budget("3"))

		first := newClaim(tenant.Name, tier.Name)
		waitReady(first.Name)

		second := newClaim(tenant.Name, tier.Name)
		refused := waitRefused(second.Name, platformv1alpha1.PhasePending, ReasonQuotaBudgetExceeded)
		message := meta.FindStatusCondition(refused.Status.Conditions, platformv1alpha1.ConditionAdmitted).Message
		Expect(message).To(ContainSubstring("requests.cpu: wants 2, 1 free of 3"))
		Expect(exists(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: second.Name}})).To(BeFalse())

		Eventually(func() error {
			t := getTenant(tenant.Name)
			t.Spec.QuotaBudget[corev1.ResourceRequestsCPU] = qty("4")
			return k8sClient.Update(ctx, t)
		}).Should(Succeed())
		waitReady(second.Name)

		allocated := getTenant(tenant.Name).Status.Allocated[corev1.ResourceRequestsCPU]
		Expect(allocated.Cmp(qty("4"))).To(BeZero())
	})

	It("refuses a project that has no quota for a budgeted resource", func() {
		tier := newTier()
		tenant := newTenant(func(t *platformv1alpha1.Tenant) {
			t.Spec.QuotaBudget = corev1.ResourceList{corev1.ResourceLimitsCPU: qty("100")}
		})
		// The tier sets no limits.cpu. Admitting this would leave the
		// project free to use any amount of a resource the tenant budgets.
		claim := newClaim(tenant.Name, tier.Name)
		waitRefused(claim.Name, platformv1alpha1.PhasePending, ReasonBudgetResourceUnbounded)
	})

	It("keeps a running project as it is when a later increase does not fit", func() {
		tier := newTier()
		tenant := newTenant(budget("3"))
		claim := newClaim(tenant.Name, tier.Name)
		waitReady(claim.Name)

		updateClaim(claim.Name, func(c *platformv1alpha1.ProjectClaim) {
			c.Spec.Quota = corev1.ResourceList{corev1.ResourceRequestsCPU: qty("10")}
		})
		degraded := waitRefused(claim.Name, platformv1alpha1.PhaseDegraded, ReasonQuotaBudgetExceeded)
		ready := meta.FindStatusCondition(degraded.Status.Conditions, platformv1alpha1.ConditionReady)
		Expect(ready.Reason).To(Equal(ReasonSpecNotApplied))

		Consistently(func(g Gomega) {
			quota := &corev1.ResourceQuota{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: claim.Name, Name: QuotaName}, quota)).To(Succeed())
			cpu := quota.Spec.Hard[corev1.ResourceRequestsCPU]
			g.Expect(cpu.Cmp(qty("2"))).To(BeZero(), "the namespace keeps the quota that was admitted")

			held := reservationOf(getTenant(tenant.Name), claim.Name)
			g.Expect(held).NotTo(BeNil())
			reserved := held.Quota[corev1.ResourceRequestsCPU]
			g.Expect(reserved.Cmp(qty("2"))).To(BeZero(), "and so does the ledger")
		}).Should(Succeed())

		By("recovering when the claim asks for something that fits")
		updateClaim(claim.Name, func(c *platformv1alpha1.ProjectClaim) {
			c.Spec.Quota = corev1.ResourceList{corev1.ResourceRequestsCPU: qty("3")}
		})
		waitReady(claim.Name)
	})

	It("never overbooks when many claims arrive at once", func() {
		const claims, fit = 12, 4
		tier := newTier()                // 2 CPU per project
		tenant := newTenant(budget("8")) // room for exactly four

		names := make([]string, 0, claims)
		for i := 0; i < claims; i++ {
			names = append(names, newClaim(tenant.Name, tier.Name).Name)
		}

		count := func() (ready, waiting int) {
			for _, name := range names {
				claim := getClaim(name)
				admitted := meta.FindStatusCondition(claim.Status.Conditions, platformv1alpha1.ConditionAdmitted)
				switch {
				case IsReady(claim):
					ready++
				case admitted != nil && admitted.Reason == ReasonQuotaBudgetExceeded:
					waiting++
				}
			}
			return ready, waiting
		}

		// Four workers reconcile these in parallel. With a budget check
		// against the cache, more than four would slip through.
		Eventually(func(g Gomega) {
			ready, waiting := count()
			g.Expect(ready).To(Equal(fit))
			g.Expect(waiting).To(Equal(claims - fit))
		}).Should(Succeed())

		Consistently(func(g Gomega) {
			ready, _ := count()
			g.Expect(ready).To(Equal(fit))
			t := getTenant(tenant.Name)
			g.Expect(t.Status.ProjectCount).To(BeEquivalentTo(fit))
			allocated := t.Status.Allocated[corev1.ResourceRequestsCPU]
			g.Expect(allocated.Cmp(qty("8"))).To(BeZero())
		}).Should(Succeed())

		namespaces := 0
		for _, name := range names {
			if exists(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}) {
				namespaces++
			}
		}
		Expect(namespaces).To(Equal(fit), "only admitted claims get a namespace")
	})
})

var _ = Describe("Tenant ownership (admission policy)", func() {
	var (
		tier   *platformv1alpha1.ProjectTier
		tenant *platformv1alpha1.Tenant
	)

	BeforeEach(func() {
		tier = newTier()
		tenant = newTenant(func(t *platformv1alpha1.Tenant) {
			t.Spec.Owners = []string{"alice"}
			t.Spec.OwnerGroups = []string{"team-payments"}
		})
	})

	It("rejects a claim from someone who is not an owner of the tenant", func() {
		mallory := asUser("mallory")
		// The API server needs a moment to start enforcing a new policy, so
		// keep trying. Dry-run keeps the early attempts from creating anything.
		Eventually(func() string {
			err := mallory.Create(ctx, claimFor(tenant.Name, tier.Name), client.DryRunAll)
			if err == nil {
				return ""
			}
			return err.Error()
		}).Should(ContainSubstring(`user "mallory" is not an owner of tenant "` + tenant.Name + `"`))
	})

	It("accepts claims from a listed owner and from a member of an owner group", func() {
		Expect(asUser("alice").Create(ctx, claimFor(tenant.Name, tier.Name), client.DryRunAll)).To(Succeed())
		Expect(asUser("bob", "team-payments").Create(ctx, claimFor(tenant.Name, tier.Name), client.DryRunAll)).To(Succeed())
	})

	It("lets an owner of one tenant only touch that tenant", func() {
		other := newTenant(func(t *platformv1alpha1.Tenant) { t.Spec.Owners = []string{"carol"} })
		alice := asUser("alice")
		Eventually(func() error {
			return alice.Create(ctx, claimFor(other.Name, tier.Name), client.DryRunAll)
		}).ShouldNot(Succeed())
		Expect(alice.Create(ctx, claimFor(tenant.Name, tier.Name), client.DryRunAll)).To(Succeed())
	})

	It("lets through whoever holds the administer verb on the tenant, owner or not", func() {
		// This is how platform admins and the operator itself get past the
		// policy: through RBAC, not through a name written into the policy.
		role := &rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{Name: uniq("tenant-admin")},
			Rules: []rbacv1.PolicyRule{{
				APIGroups:     []string{platformv1alpha1.GroupVersion.Group},
				Resources:     []string{"tenants"},
				Verbs:         []string{"administer"},
				ResourceNames: []string{tenant.Name},
			}},
		}
		Expect(k8sClient.Create(ctx, role)).To(Succeed())
		Expect(k8sClient.Create(ctx, &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: role.Name},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
			Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: "dana"}},
		})).To(Succeed())

		dana := asUser("dana")
		Eventually(func() error {
			return dana.Create(ctx, claimFor(tenant.Name, tier.Name), client.DryRunAll)
		}).Should(Succeed())

		By("but only for that tenant")
		other := newTenant()
		Eventually(func() error {
			return dana.Create(ctx, claimFor(other.Name, tier.Name), client.DryRunAll)
		}).ShouldNot(Succeed())
	})

	It("stops a non-owner from editing an existing claim", func() {
		claim := newClaim(tenant.Name, tier.Name)
		waitReady(claim.Name)

		mallory := asUser("mallory")
		Eventually(func() error {
			fresh := &platformv1alpha1.ProjectClaim{}
			if err := mallory.Get(ctx, client.ObjectKeyFromObject(claim), fresh); err != nil {
				return nil
			}
			fresh.Spec.Access.Requester = "mallory"
			return mallory.Update(ctx, fresh, client.DryRunAll)
		}).ShouldNot(Succeed())
	})
})
