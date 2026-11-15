package controller

import (
	"context"
	"fmt"
	"sync"

	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	platformv1alpha1 "github.com/mella7/project-claim/api/v1alpha1"
)

// uniq keeps specs independent. envtest never really deletes a namespace, so
// every spec works under names of its own.
func uniq(prefix string) string {
	return fmt.Sprintf("%s-%s", prefix, rand.String(5))
}

func qty(s string) resource.Quantity { return resource.MustParse(s) }

// newTier creates a tier with 2 CPU / 4Gi of requests per project.
func newTier(mods ...func(*platformv1alpha1.ProjectTier)) *platformv1alpha1.ProjectTier {
	tier := &platformv1alpha1.ProjectTier{
		ObjectMeta: metav1.ObjectMeta{Name: uniq("tier")},
		Spec: platformv1alpha1.ProjectTierSpec{
			Quota: corev1.ResourceList{
				corev1.ResourceRequestsCPU:    qty("2"),
				corev1.ResourceRequestsMemory: qty("4Gi"),
				corev1.ResourcePods:           qty("20"),
			},
			ContainerDefaults: platformv1alpha1.ContainerDefaults{
				Limits:   corev1.ResourceList{corev1.ResourceCPU: qty("500m"), corev1.ResourceMemory: qty("512Mi")},
				Requests: corev1.ResourceList{corev1.ResourceCPU: qty("50m")},
			},
		},
	}
	for _, mod := range mods {
		mod(tier)
	}
	ExpectWithOffset(1, k8sClient.Create(ctx, tier)).To(Succeed())
	return tier
}

func newTenant(mods ...func(*platformv1alpha1.Tenant)) *platformv1alpha1.Tenant {
	tenant := &platformv1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: uniq("tenant")}}
	for _, mod := range mods {
		mod(tenant)
	}
	ExpectWithOffset(1, k8sClient.Create(ctx, tenant)).To(Succeed())
	return tenant
}

// claimFor builds a claim without creating it.
func claimFor(tenant, tier string, mods ...func(*platformv1alpha1.ProjectClaim)) *platformv1alpha1.ProjectClaim {
	name := uniq("proj")
	claim := &platformv1alpha1.ProjectClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: platformv1alpha1.ProjectClaimSpec{
			ProjectName: name,
			TenantRef:   platformv1alpha1.TenantReference{Name: tenant},
			Tier:        tier,
			Access:      platformv1alpha1.AccessSpec{Requester: "alice@example.com"},
		},
	}
	for _, mod := range mods {
		mod(claim)
	}
	return claim
}

func newClaim(tenant, tier string, mods ...func(*platformv1alpha1.ProjectClaim)) *platformv1alpha1.ProjectClaim {
	claim := claimFor(tenant, tier, mods...)
	ExpectWithOffset(1, k8sClient.Create(ctx, claim)).To(Succeed())
	return claim
}

func getClaim(name string) *platformv1alpha1.ProjectClaim {
	claim := &platformv1alpha1.ProjectClaim{}
	ExpectWithOffset(1, k8sClient.Get(ctx, client.ObjectKey{Name: name}, claim)).To(Succeed())
	return claim
}

func getTenant(name string) *platformv1alpha1.Tenant {
	tenant := &platformv1alpha1.Tenant{}
	ExpectWithOffset(1, k8sClient.Get(ctx, client.ObjectKey{Name: name}, tenant)).To(Succeed())
	return tenant
}

// waitReady blocks until the claim is Ready for its current spec.
func waitReady(name string) *platformv1alpha1.ProjectClaim {
	claim := &platformv1alpha1.ProjectClaim{}
	EventuallyWithOffset(1, func(g Gomega) {
		g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, claim)).To(Succeed())
		g.Expect(IsReady(claim)).To(BeTrue(), "phase=%s conditions=%v", claim.Status.Phase, claim.Status.Conditions)
	}).Should(Succeed())
	return claim
}

// waitRefused blocks until the claim reports the given phase and the given
// reason on its Admitted condition.
func waitRefused(name string, phase platformv1alpha1.Phase, reason string) *platformv1alpha1.ProjectClaim {
	claim := &platformv1alpha1.ProjectClaim{}
	EventuallyWithOffset(1, func(g Gomega) {
		g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, claim)).To(Succeed())
		admitted := meta.FindStatusCondition(claim.Status.Conditions, platformv1alpha1.ConditionAdmitted)
		g.Expect(admitted).NotTo(BeNil())
		g.Expect(admitted.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(admitted.Reason).To(Equal(reason))
		g.Expect(claim.Status.Phase).To(Equal(phase))
		g.Expect(claim.Status.ObservedGeneration).To(Equal(claim.Generation))
	}).Should(Succeed())
	return claim
}

// updateClaim applies a change to the latest version of the claim, retrying
// when the operator wrote in between.
func updateClaim(name string, change func(*platformv1alpha1.ProjectClaim)) {
	EventuallyWithOffset(1, func() error {
		claim := &platformv1alpha1.ProjectClaim{}
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: name}, claim); err != nil {
			return err
		}
		change(claim)
		return k8sClient.Update(ctx, claim)
	}).Should(Succeed())
}

func exists(obj client.Object) bool {
	err := k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), obj)
	if apierrors.IsNotFound(err) {
		return false
	}
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return true
}

// finalizeNamespace plays the part of the namespace controller, which envtest
// does not run: it clears spec.finalizers through the finalize subresource,
// and the API server then removes a namespace that is marked for deletion.
func finalizeNamespace(name string) {
	EventuallyWithOffset(1, func(g Gomega) {
		ns := &corev1.Namespace{}
		err := k8sClient.Get(ctx, client.ObjectKey{Name: name}, ns)
		if apierrors.IsNotFound(err) {
			return
		}
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(ns.DeletionTimestamp.IsZero()).To(BeFalse(), "namespace is not being deleted")
		ns.Spec.Finalizers = nil
		g.Expect(k8sClient.SubResource("finalize").Update(ctx, ns)).To(Succeed())
		g.Expect(apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKey{Name: name}, ns))).To(BeTrue())
	}).Should(Succeed())
}

// asUser returns a client that acts as the given user, through impersonation.
func asUser(username string, groups ...string) client.Client {
	impersonated := rest.CopyConfig(cfg)
	impersonated.Impersonate = rest.ImpersonationConfig{
		UserName: username,
		Groups:   append([]string{"system:authenticated", claimAuthors}, groups...),
	}
	c, err := client.New(impersonated, client.Options{Scheme: k8sClient.Scheme()})
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return c
}

func reservationOf(tenant *platformv1alpha1.Tenant, claimName string) *platformv1alpha1.Reservation {
	for i := range tenant.Status.Reservations {
		if tenant.Status.Reservations[i].ClaimName == claimName {
			return &tenant.Status.Reservations[i]
		}
	}
	return nil
}

// write is one mutating call the operator made.
type write struct {
	Verb      string
	Kind      string
	Namespace string
	Name      string
}

// recordingClient wraps the operator's client and notes every call that
// writes. Comparing resourceVersions is not enough to prove that a reconcile
// wrote nothing: the API server drops an update that changes nothing without
// bumping the version, so a wasteful operator would look clean. Counting the
// calls themselves leaves no such gap.
type recordingClient struct {
	client.Client
	mu     sync.Mutex
	writes []write
}

func (c *recordingClient) note(verb string, obj client.Object) {
	kind := fmt.Sprintf("%T", obj)
	if gvk, err := c.GroupVersionKindFor(obj); err == nil {
		kind = gvk.Kind
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writes = append(c.writes, write{Verb: verb, Kind: kind, Namespace: obj.GetNamespace(), Name: obj.GetName()})
}

// mark returns a position in the log to compare against later.
func (c *recordingClient) mark() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.writes)
}

// since returns the writes after mark that touched one of the given names,
// either as an object name or as a namespace.
func (c *recordingClient) since(mark int, names ...string) []write {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []write
	for _, w := range c.writes[mark:] {
		for _, n := range names {
			if w.Name == n || w.Namespace == n {
				out = append(out, w)
				break
			}
		}
	}
	return out
}

func (c *recordingClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	c.note("create", obj)
	return c.Client.Create(ctx, obj, opts...)
}

func (c *recordingClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	c.note("update", obj)
	return c.Client.Update(ctx, obj, opts...)
}

func (c *recordingClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	c.note("patch", obj)
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func (c *recordingClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	c.note("delete", obj)
	return c.Client.Delete(ctx, obj, opts...)
}

func (c *recordingClient) Status() client.SubResourceWriter {
	return &recordingStatus{SubResourceWriter: c.Client.Status(), parent: c}
}

type recordingStatus struct {
	client.SubResourceWriter
	parent *recordingClient
}

func (s *recordingStatus) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	s.parent.note("update status", obj)
	return s.SubResourceWriter.Update(ctx, obj, opts...)
}

func (s *recordingStatus) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	s.parent.note("patch status", obj)
	return s.SubResourceWriter.Patch(ctx, obj, patch, opts...)
}

// reconcileCount reads how many ProjectClaim reconciles have finished, from
// the counter controller-runtime keeps.
func reconcileCount() float64 {
	families, err := ctrlmetrics.Registry.Gather()
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	total := 0.0
	for _, family := range families {
		if family.GetName() != "controller_runtime_reconcile_total" {
			continue
		}
		for _, m := range family.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "controller" && l.GetValue() == "projectclaim" {
					total += m.GetCounter().GetValue()
				}
			}
		}
	}
	return total
}
