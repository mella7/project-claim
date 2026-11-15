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

var _ = Describe("Deletion", func() {
	It("holds the claim until the namespace is really gone, then releases the budget", func() {
		tier, tenant := newTier(), newTenant()
		claim := newClaim(tenant.Name, tier.Name)
		waitReady(claim.Name)

		Expect(k8sClient.Delete(ctx, claim)).To(Succeed())

		By("asking for the namespace to be deleted")
		Eventually(func(g Gomega) {
			ns := &corev1.Namespace{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: claim.Name}, ns)).To(Succeed())
			g.Expect(ns.DeletionTimestamp.IsZero()).To(BeFalse())
		}).Should(Succeed())

		// envtest has no namespace controller, so the namespace now sits in
		// Terminating, the way a namespace does in a real cluster when
		// something in it will not go away. The claim has to wait with it.
		// If the finalizer came off at "delete requested", the claim would
		// be gone here and this would fail.
		By("keeping the claim, its finalizer and its reservation while the namespace is terminating")
		Consistently(func(g Gomega) {
			fresh := getClaim(claim.Name)
			g.Expect(fresh.DeletionTimestamp.IsZero()).To(BeFalse())
			g.Expect(fresh.Finalizers).To(ContainElement(Finalizer))
			g.Expect(fresh.Status.Phase).To(Equal(platformv1alpha1.PhaseTerminating))
			g.Expect(reservationOf(getTenant(tenant.Name), claim.Name)).NotTo(BeNil())
		}, 3*time.Second).Should(Succeed())

		By("letting go once the namespace has gone")
		finalizeNamespace(claim.Name)
		Eventually(func() bool {
			return exists(&platformv1alpha1.ProjectClaim{ObjectMeta: metav1.ObjectMeta{Name: claim.Name}})
		}).Should(BeFalse())

		t := getTenant(tenant.Name)
		Expect(reservationOf(t, claim.Name)).To(BeNil())
		Expect(t.Status.ProjectCount).To(BeZero())
		Expect(t.Status.Allocated).To(BeEmpty())
	})

	It("keeps the namespace and detaches it when the reclaim policy is Retain", func() {
		tier, tenant := newTier(), newTenant()
		claim := newClaim(tenant.Name, tier.Name, func(c *platformv1alpha1.ProjectClaim) {
			c.Spec.ReclaimPolicy = platformv1alpha1.ReclaimRetain
			c.Spec.Access.EditorGroups = []string{"devs"}
		})
		uid := waitReady(claim.Name).UID

		Expect(k8sClient.Delete(ctx, claim)).To(Succeed())
		Eventually(func() bool {
			return exists(&platformv1alpha1.ProjectClaim{ObjectMeta: metav1.ObjectMeta{Name: claim.Name}})
		}).Should(BeFalse())

		ns := &corev1.Namespace{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: claim.Name}, ns)).To(Succeed())
		Expect(ns.DeletionTimestamp.IsZero()).To(BeTrue())
		Expect(ns.Annotations).To(HaveKeyWithValue(AnnotationReleasedFrom, claim.Name))

		// Any reference left behind would point at an owner that no longer
		// exists, and the garbage collector deletes such objects.
		kept := []client.Object{
			ns,
			&corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Namespace: claim.Name, Name: QuotaName}},
			&corev1.LimitRange{ObjectMeta: metav1.ObjectMeta{Namespace: claim.Name, Name: LimitRangeName}},
			&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Namespace: claim.Name, Name: AdminBindingName}},
			&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Namespace: claim.Name, Name: EditBindingName}},
			&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: claim.Name, Name: NetworkPolicyName}},
		}
		for _, obj := range kept {
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), obj)).To(Succeed())
			for _, ref := range obj.GetOwnerReferences() {
				Expect(ref.UID).NotTo(Equal(uid), "%T %s still points at the deleted claim", obj, obj.GetName())
			}
		}

		Expect(reservationOf(getTenant(tenant.Name), claim.Name)).To(BeNil())
	})

	It("cleans up the reservation of a claim that was removed behind its back", func() {
		tier, tenant := newTier(), newTenant()
		claim := newClaim(tenant.Name, tier.Name)
		waitReady(claim.Name)
		Expect(reservationOf(getTenant(tenant.Name), claim.Name)).NotTo(BeNil())

		// The escape hatch people reach for when a delete hangs: strip the
		// finalizer by hand. The claim's own cleanup never runs, so its
		// reservation is left behind.
		Expect(k8sClient.Delete(ctx, claim)).To(Succeed())
		updateClaim(claim.Name, func(c *platformv1alpha1.ProjectClaim) { c.Finalizers = nil })

		Eventually(func(g Gomega) {
			g.Expect(exists(&platformv1alpha1.ProjectClaim{ObjectMeta: metav1.ObjectMeta{Name: claim.Name}})).To(BeFalse())
			t := getTenant(tenant.Name)
			g.Expect(reservationOf(t, claim.Name)).To(BeNil())
			g.Expect(t.Status.ProjectCount).To(BeZero())
		}).Should(Succeed())
	})
})

var _ = Describe("Lifetime", func() {
	It("deletes a claim when its TTL runs out", func() {
		tier, tenant := newTier(), newTenant()
		claim := newClaim(tenant.Name, tier.Name, func(c *platformv1alpha1.ProjectClaim) {
			c.Spec.TTL = &metav1.Duration{Duration: 4 * time.Second}
		})

		ready := waitReady(claim.Name)
		Expect(ready.Status.ExpiresAt).NotTo(BeNil())
		Expect(ready.Status.ExpiresAt.Time).To(BeTemporally("~", ready.CreationTimestamp.Add(4*time.Second), time.Second))

		Eventually(func(g Gomega) {
			g.Expect(getClaim(claim.Name).DeletionTimestamp.IsZero()).To(BeFalse())
		}).Should(Succeed())
		Expect(time.Now()).To(BeTemporally(">=", ready.Status.ExpiresAt.Time), "not before its time")
	})

	It("applies the tier's default TTL to claims that set none", func() {
		tier := newTier(func(t *platformv1alpha1.ProjectTier) {
			t.Spec.DefaultTTL = &metav1.Duration{Duration: time.Hour}
		})
		tenant := newTenant()
		claim := newClaim(tenant.Name, tier.Name)

		ready := waitReady(claim.Name)
		Expect(ready.Status.ExpiresAt).NotTo(BeNil())
		Expect(ready.Status.ExpiresAt.Time).To(BeTemporally("~", ready.CreationTimestamp.Add(time.Hour), time.Second))
	})
})
