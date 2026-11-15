package controller

import (
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	platformv1alpha1 "github.com/mella7/project-claim/api/v1alpha1"
)

// This file holds the pure half of the operator: functions that turn a claim
// and its tier into desired state. They do no I/O, so they are cheap to unit
// test, and they are deterministic, which is what keeps repeated reconciles
// from writing anything.

// EffectiveQuota merges the claim's overrides over the tier's quota, key by
// key. A claim that only raises `pods` keeps every other value of its tier.
func EffectiveQuota(tier *platformv1alpha1.ProjectTier, claim *platformv1alpha1.ProjectClaim) corev1.ResourceList {
	out := corev1.ResourceList{}
	for k, v := range tier.Spec.Quota {
		out[k] = v.DeepCopy()
	}
	for k, v := range claim.Spec.Quota {
		out[k] = v.DeepCopy()
	}
	return out
}

func podSecurityLabels(tier *platformv1alpha1.ProjectTier) map[string]string {
	ps := tier.Spec.PodSecurity
	if ps.Enforce == "" {
		ps.Enforce = platformv1alpha1.PodSecurityBaseline
	}
	if ps.Audit == "" {
		ps.Audit = platformv1alpha1.PodSecurityRestricted
	}
	if ps.Warn == "" {
		ps.Warn = platformv1alpha1.PodSecurityRestricted
	}
	return map[string]string{
		LabelPodSecurityEnforce: string(ps.Enforce),
		LabelPodSecurityAudit:   string(ps.Audit),
		LabelPodSecurityWarn:    string(ps.Warn),
	}
}

// childLabels go on every object the operator creates inside the namespace.
func childLabels(claim *platformv1alpha1.ProjectClaim) map[string]string {
	return map[string]string{
		LabelManagedBy: ManagerName,
		LabelClaim:     claim.Name,
		LabelTenant:    claim.Spec.TenantRef.Name,
		LabelTier:      claim.Spec.Tier,
	}
}

// namespaceLabels is the full set of labels the operator wants on the
// namespace. Operator labels are written last so they always win.
func namespaceLabels(claim *platformv1alpha1.ProjectClaim, tier *platformv1alpha1.ProjectTier) map[string]string {
	out := map[string]string{}
	for k, v := range claim.Spec.ExtraLabels {
		if !reservedLabel(k) {
			out[k] = v
		}
	}
	for k, v := range podSecurityLabels(tier) {
		out[k] = v
	}
	for k, v := range childLabels(claim) {
		out[k] = v
	}
	return out
}

// mutateNamespace applies the desired labels and annotations on top of
// whatever the namespace already carries. Labels set by other parties are
// left alone. Labels the operator set earlier and no longer wants are
// removed, which the managed-labels annotation makes possible.
func mutateNamespace(ns *corev1.Namespace, claim *platformv1alpha1.ProjectClaim, tier *platformv1alpha1.ProjectTier) {
	desired := namespaceLabels(claim, tier)

	if ns.Labels == nil {
		ns.Labels = map[string]string{}
	}
	if ns.Annotations == nil {
		ns.Annotations = map[string]string{}
	}

	for _, k := range splitList(ns.Annotations[AnnotationManagedLabels]) {
		if _, still := desired[k]; !still {
			delete(ns.Labels, k)
		}
	}
	keys := make([]string, 0, len(desired))
	for k, v := range desired {
		ns.Labels[k] = v
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ns.Annotations[AnnotationManagedLabels] = strings.Join(keys, ",")

	ns.Annotations[AnnotationDisplayName] = claim.Spec.ProjectName
	ns.Annotations[AnnotationRequester] = claim.Spec.Access.Requester
	if claim.Spec.Description != "" {
		ns.Annotations[AnnotationDescription] = claim.Spec.Description
	} else {
		delete(ns.Annotations, AnnotationDescription)
	}
	delete(ns.Annotations, AnnotationReleasedFrom)
}

func splitList(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// hasContainerDefaults reports whether the tier asks for a LimitRange at all.
func hasContainerDefaults(tier *platformv1alpha1.ProjectTier) bool {
	d := tier.Spec.ContainerDefaults
	return len(d.Limits) > 0 || len(d.Requests) > 0
}

// limitRangeSpec builds the LimitRange from the tier's container defaults.
//
// The API server defaults a missing default request to the default limit. If
// this function left that out, the stored object would never compare equal
// to the desired one, and the operator would send an update on every
// reconcile. The server would discard each one as a no-op, so nothing would
// look wrong, but it is a write per project per reconcile for nothing.
// Applying the same rule here makes the comparison come out equal.
func limitRangeSpec(tier *platformv1alpha1.ProjectTier) corev1.LimitRangeSpec {
	limits := corev1.ResourceList{}
	for k, v := range tier.Spec.ContainerDefaults.Limits {
		limits[k] = v.DeepCopy()
	}
	requests := corev1.ResourceList{}
	for k, v := range tier.Spec.ContainerDefaults.Requests {
		requests[k] = v.DeepCopy()
	}
	for k, v := range limits {
		if _, ok := requests[k]; !ok {
			requests[k] = v.DeepCopy()
		}
	}
	return corev1.LimitRangeSpec{
		Limits: []corev1.LimitRangeItem{{
			Type:           corev1.LimitTypeContainer,
			Default:        limits,
			DefaultRequest: requests,
		}},
	}
}

// roleBinding describes one of the bindings the operator maintains.
type roleBinding struct {
	Name        string
	ClusterRole string
	Subjects    []rbacv1.Subject
}

// desiredRoleBindings returns all three bindings. One with no subjects is
// still returned so the caller knows to delete it if it exists.
func desiredRoleBindings(claim *platformv1alpha1.ProjectClaim) []roleBinding {
	groups := func(names []string) []rbacv1.Subject {
		sorted := append([]string(nil), names...)
		sort.Strings(sorted)
		out := make([]rbacv1.Subject, 0, len(sorted))
		for _, g := range sorted {
			out = append(out, rbacv1.Subject{APIGroup: rbacv1.GroupName, Kind: rbacv1.GroupKind, Name: g})
		}
		return out
	}
	return []roleBinding{
		{
			Name:        AdminBindingName,
			ClusterRole: "admin",
			Subjects: []rbacv1.Subject{{
				APIGroup: rbacv1.GroupName,
				Kind:     rbacv1.UserKind,
				Name:     claim.Spec.Access.Requester,
			}},
		},
		{Name: EditBindingName, ClusterRole: "edit", Subjects: groups(claim.Spec.Access.EditorGroups)},
		{Name: ViewBindingName, ClusterRole: "view", Subjects: groups(claim.Spec.Access.ViewerGroups)},
	}
}

// networkPolicySpec denies all ingress except from the namespace itself and
// from the namespaces OpenShift labels as ingress and monitoring. A plain
// default-deny would cut off Routes and Prometheus scraping.
func networkPolicySpec(claim *platformv1alpha1.ProjectClaim) networkingv1.NetworkPolicySpec {
	fromNamespace := func(key, value string) networkingv1.NetworkPolicyPeer {
		return networkingv1.NetworkPolicyPeer{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{key: value}},
		}
	}
	return networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{},
		PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		Ingress: []networkingv1.NetworkPolicyIngressRule{{
			From: []networkingv1.NetworkPolicyPeer{
				fromNamespace(corev1.LabelMetadataName, claim.Spec.ProjectName),
				fromNamespace("network.openshift.io/policy-group", "ingress"),
				fromNamespace("network.openshift.io/policy-group", "monitoring"),
			},
		}},
	}
}
