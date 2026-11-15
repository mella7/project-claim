package controller

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	platformv1alpha1 "github.com/mella7/project-claim/api/v1alpha1"
)

var _ = Describe("Provisioning", Ordered, func() {
	var (
		tier   *platformv1alpha1.ProjectTier
		tenant *platformv1alpha1.Tenant
		name   string
	)

	BeforeAll(func() {
		tier = newTier(func(t *platformv1alpha1.ProjectTier) {
			t.Spec.PodSecurity.Enforce = platformv1alpha1.PodSecurityRestricted
		})
		tenant = newTenant()
		claim := newClaim(tenant.Name, tier.Name, func(c *platformv1alpha1.ProjectClaim) {
			c.Spec.Description = "payments staging"
			c.Spec.Access.EditorGroups = []string{"payments-devs"}
			c.Spec.Access.ViewerGroups = []string{"auditors"}
			c.Spec.Quota = corev1.ResourceList{corev1.ResourcePods: qty("80")}
			c.Spec.ExtraLabels = map[string]string{"cost-center": "cc-4412"}
		})
		name = claim.Name
		waitReady(name)
	})

	It("creates the namespace with its labels, Pod Security levels and OpenShift annotations", func() {
		ns := &corev1.Namespace{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, ns)).To(Succeed())

		Expect(ns.Labels).To(HaveKeyWithValue(LabelManagedBy, ManagerName))
		Expect(ns.Labels).To(HaveKeyWithValue(LabelClaim, name))
		Expect(ns.Labels).To(HaveKeyWithValue(LabelTenant, tenant.Name))
		Expect(ns.Labels).To(HaveKeyWithValue(LabelTier, tier.Name))
		Expect(ns.Labels).To(HaveKeyWithValue("cost-center", "cc-4412"))

		Expect(ns.Labels).To(HaveKeyWithValue(LabelPodSecurityEnforce, "restricted"))
		// Not set on the tier, so these come from the CRD defaults.
		Expect(ns.Labels).To(HaveKeyWithValue(LabelPodSecurityAudit, "restricted"))
		Expect(ns.Labels).To(HaveKeyWithValue(LabelPodSecurityWarn, "restricted"))

		Expect(ns.Annotations).To(HaveKeyWithValue(AnnotationRequester, "alice@example.com"))
		Expect(ns.Annotations).To(HaveKeyWithValue(AnnotationDescription, "payments staging"))
		Expect(ns.Annotations).To(HaveKeyWithValue(AnnotationDisplayName, name))

		owner := metav1.GetControllerOf(ns)
		Expect(owner).NotTo(BeNil())
		Expect(owner.UID).To(Equal(getClaim(name).UID))
	})

	It("merges the claim's quota overrides over the tier, key by key", func() {
		quota := &corev1.ResourceQuota{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: name, Name: QuotaName}, quota)).To(Succeed())

		pods := quota.Spec.Hard[corev1.ResourcePods]
		cpu := quota.Spec.Hard[corev1.ResourceRequestsCPU]
		Expect(pods.Cmp(qty("80"))).To(BeZero(), "the override wins")
		Expect(cpu.Cmp(qty("2"))).To(BeZero(), "keys the claim does not mention keep the tier value")
	})

	It("creates the limit range from the tier's container defaults", func() {
		limits := &corev1.LimitRange{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: name, Name: LimitRangeName}, limits)).To(Succeed())
		Expect(limits.Spec.Limits).To(HaveLen(1))

		item := limits.Spec.Limits[0]
		cpuLimit, cpuRequest := item.Default[corev1.ResourceCPU], item.DefaultRequest[corev1.ResourceCPU]
		memRequest := item.DefaultRequest[corev1.ResourceMemory]
		Expect(cpuLimit.Cmp(qty("500m"))).To(BeZero())
		Expect(cpuRequest.Cmp(qty("50m"))).To(BeZero())
		Expect(memRequest.Cmp(qty("512Mi"))).To(BeZero(), "a missing default request falls back to the limit")
	})

	It("binds the requester to admin and the groups to edit and view", func() {
		for binding, want := range map[string]struct{ role, kind, subject string }{
			AdminBindingName: {"admin", rbacv1.UserKind, "alice@example.com"},
			EditBindingName:  {"edit", rbacv1.GroupKind, "payments-devs"},
			ViewBindingName:  {"view", rbacv1.GroupKind, "auditors"},
		} {
			rb := &rbacv1.RoleBinding{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: name, Name: binding}, rb)).To(Succeed())
			Expect(rb.RoleRef.Kind).To(Equal("ClusterRole"))
			Expect(rb.RoleRef.Name).To(Equal(want.role))
			Expect(rb.Subjects).To(HaveLen(1))
			Expect(rb.Subjects[0].Kind).To(Equal(want.kind))
			Expect(rb.Subjects[0].Name).To(Equal(want.subject))
		}
	})

	It("isolates the namespace but keeps ingress and monitoring working", func() {
		policy := &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: name, Name: NetworkPolicyName}, policy)).To(Succeed())
		Expect(policy.Spec.PolicyTypes).To(ConsistOf(networkingv1.PolicyTypeIngress))
		Expect(policy.Spec.Ingress).To(HaveLen(1))
		Expect(policy.Spec.Ingress[0].From).To(HaveLen(3))
	})

	It("reports the result on the claim and books it on the tenant", func() {
		claim := getClaim(name)
		Expect(claim.Status.Phase).To(Equal(platformv1alpha1.PhaseReady))
		Expect(claim.Status.NamespaceName).To(Equal(name))
		Expect(claim.Finalizers).To(ContainElement(Finalizer))
		applied := claim.Status.AppliedQuota[corev1.ResourcePods]
		Expect(applied.Cmp(qty("80"))).To(BeZero())

		Eventually(func(g Gomega) {
			t := getTenant(tenant.Name)
			g.Expect(t.Status.ProjectCount).To(BeEquivalentTo(1))
			res := reservationOf(t, name)
			g.Expect(res).NotTo(BeNil())
			g.Expect(res.ClaimUID).To(Equal(claim.UID))
			allocated := t.Status.Allocated[corev1.ResourceRequestsCPU]
			g.Expect(allocated.Cmp(qty("2"))).To(BeZero())
		}).Should(Succeed())
	})

	It("writes nothing when it reconciles again", func() {
		// Let the writes of the initial provisioning finish first.
		Eventually(func() int64 { return getTenant(tenant.Name).Status.ObservedGeneration }).Should(BeEquivalentTo(1))
		statusBefore := getClaim(name).Status
		mark := operatorWrites.mark()
		reconciles := reconcileCount()

		// Each annotation change is an update event and so a full reconcile.
		const pokes = 3
		for i := 0; i < pokes; i++ {
			updateClaim(name, func(c *platformv1alpha1.ProjectClaim) {
				if c.Annotations == nil {
					c.Annotations = map[string]string{}
				}
				c.Annotations["test/poke"] = time.Now().Format(time.RFC3339Nano)
			})
			time.Sleep(300 * time.Millisecond)
		}
		Eventually(reconcileCount).Should(BeNumerically(">=", reconciles+pokes), "the pokes must actually reconcile")

		// Not one create, update, patch or delete: not on the namespace, not
		// on anything in it, not on the claim's status, not on the tenant.
		// A mutate function that is not deterministic, or a desired state
		// that disagrees with a default the API server applies, shows up here.
		Consistently(func() []write {
			return operatorWrites.since(mark, name, tenant.Name)
		}).Should(BeEmpty())
		Expect(getClaim(name).Status).To(Equal(statusBefore))
	})

	Context("when something changes the operator's objects", func() {
		It("recreates a deleted quota", func() {
			quota := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Namespace: name, Name: QuotaName}}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(quota), quota)).To(Succeed())
			oldUID := quota.UID
			Expect(k8sClient.Delete(ctx, quota)).To(Succeed())

			Eventually(func(g Gomega) {
				fresh := &corev1.ResourceQuota{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: name, Name: QuotaName}, fresh)).To(Succeed())
				g.Expect(fresh.UID).NotTo(Equal(oldUID))
			}).Should(Succeed())
		})

		It("reverts an edited quota", func() {
			quota := &corev1.ResourceQuota{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: name, Name: QuotaName}, quota)).To(Succeed())
			quota.Spec.Hard[corev1.ResourcePods] = qty("9999")
			Expect(k8sClient.Update(ctx, quota)).To(Succeed())

			Eventually(func(g Gomega) {
				fresh := &corev1.ResourceQuota{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: name, Name: QuotaName}, fresh)).To(Succeed())
				pods := fresh.Spec.Hard[corev1.ResourcePods]
				g.Expect(pods.Cmp(qty("80"))).To(BeZero())
			}).Should(Succeed())
		})

		It("reverts a lowered Pod Security level on the namespace", func() {
			ns := &corev1.Namespace{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, ns)).To(Succeed())
			ns.Labels[LabelPodSecurityEnforce] = "privileged"
			Expect(k8sClient.Update(ctx, ns)).To(Succeed())

			Eventually(func(g Gomega) {
				fresh := &corev1.Namespace{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, fresh)).To(Succeed())
				g.Expect(fresh.Labels).To(HaveKeyWithValue(LabelPodSecurityEnforce, "restricted"))
			}).Should(Succeed())
		})

		It("removes a subject that was added to the admin binding", func() {
			rb := &rbacv1.RoleBinding{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: name, Name: AdminBindingName}, rb)).To(Succeed())
			rb.Subjects = append(rb.Subjects, rbacv1.Subject{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: "mallory"})
			Expect(k8sClient.Update(ctx, rb)).To(Succeed())

			Eventually(func(g Gomega) {
				fresh := &rbacv1.RoleBinding{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: name, Name: AdminBindingName}, fresh)).To(Succeed())
				g.Expect(fresh.Subjects).To(HaveLen(1))
				g.Expect(fresh.Subjects[0].Name).To(Equal("alice@example.com"))
			}).Should(Succeed())
		})
	})

	Context("when the claim stops asking for something", func() {
		It("removes the edit binding once there are no editor groups", func() {
			updateClaim(name, func(c *platformv1alpha1.ProjectClaim) { c.Spec.Access.EditorGroups = nil })
			Eventually(func() bool {
				return exists(&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Namespace: name, Name: EditBindingName}})
			}).Should(BeFalse())
		})

		It("removes the network policy when isolation is turned off, and brings it back", func() {
			policy := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: name, Name: NetworkPolicyName}}

			updateClaim(name, func(c *platformv1alpha1.ProjectClaim) { off := false; c.Spec.NetworkIsolation = &off })
			Eventually(func() bool { return exists(policy) }).Should(BeFalse())

			updateClaim(name, func(c *platformv1alpha1.ProjectClaim) { on := true; c.Spec.NetworkIsolation = &on })
			Eventually(func() bool { return exists(policy) }).Should(BeTrue())
		})

		It("drops a namespace label that was removed from the claim, and nothing else", func() {
			ns := &corev1.Namespace{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, ns)).To(Succeed())
			ns.Labels["set-by-someone-else"] = "keep"
			Expect(k8sClient.Update(ctx, ns)).To(Succeed())

			updateClaim(name, func(c *platformv1alpha1.ProjectClaim) { c.Spec.ExtraLabels = nil })

			Eventually(func(g Gomega) {
				fresh := &corev1.Namespace{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, fresh)).To(Succeed())
				g.Expect(fresh.Labels).NotTo(HaveKey("cost-center"))
				g.Expect(fresh.Labels).To(HaveKeyWithValue("set-by-someone-else", "keep"))
			}).Should(Succeed())
		})
	})
})

var _ = Describe("Tiers", func() {
	It("holds a claim until its tier exists", func() {
		tenant := newTenant()
		tierName := uniq("tier")
		claim := newClaim(tenant.Name, tierName)

		waitRefused(claim.Name, platformv1alpha1.PhasePending, ReasonTierNotFound)
		Expect(exists(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: claim.Name}})).To(BeFalse())

		// Nobody touches the claim. The watch on ProjectTier wakes it.
		newTier(func(t *platformv1alpha1.ProjectTier) { t.Name = tierName })
		waitReady(claim.Name)
	})

	It("rolls a change to the tier out to the projects that use it", func() {
		tier, tenant := newTier(), newTenant()
		first := newClaim(tenant.Name, tier.Name)
		second := newClaim(tenant.Name, tier.Name)
		waitReady(first.Name)
		waitReady(second.Name)

		Eventually(func() error {
			fresh := &platformv1alpha1.ProjectTier{}
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(tier), fresh); err != nil {
				return err
			}
			fresh.Spec.Quota[corev1.ResourcePods] = qty("35")
			fresh.Spec.PodSecurity.Enforce = platformv1alpha1.PodSecurityRestricted
			return k8sClient.Update(ctx, fresh)
		}).Should(Succeed())

		for _, name := range []string{first.Name, second.Name} {
			Eventually(func(g Gomega) {
				quota := &corev1.ResourceQuota{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: name, Name: QuotaName}, quota)).To(Succeed())
				pods := quota.Spec.Hard[corev1.ResourcePods]
				g.Expect(pods.Cmp(qty("35"))).To(BeZero())

				ns := &corev1.Namespace{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, ns)).To(Succeed())
				g.Expect(ns.Labels).To(HaveKeyWithValue(LabelPodSecurityEnforce, "restricted"))
			}).Should(Succeed())
		}
	})

	It("refuses to turn isolation off on a tier that requires it", func() {
		tier := newTier(func(t *platformv1alpha1.ProjectTier) { t.Spec.RequireNetworkIsolation = true })
		tenant := newTenant()
		claim := newClaim(tenant.Name, tier.Name, func(c *platformv1alpha1.ProjectClaim) {
			off := false
			c.Spec.NetworkIsolation = &off
		})
		waitRefused(claim.Name, platformv1alpha1.PhasePending, ReasonNetworkIsolationRequired)
	})

	It("creates no limit range for a tier without container defaults", func() {
		tier := newTier(func(t *platformv1alpha1.ProjectTier) {
			t.Spec.ContainerDefaults = platformv1alpha1.ContainerDefaults{}
		})
		tenant := newTenant()
		claim := newClaim(tenant.Name, tier.Name)
		waitReady(claim.Name)
		Expect(exists(&corev1.LimitRange{ObjectMeta: metav1.ObjectMeta{Namespace: claim.Name, Name: LimitRangeName}})).To(BeFalse())
	})
})
