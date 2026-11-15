package controller

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	platformv1alpha1 "github.com/mella7/project-claim/api/v1alpha1"
)

// The suite runs both controllers against envtest: a real kube-apiserver and
// etcd, started locally. Admission, defaulting, CEL validation, the status
// subresource, finalizers and watches all behave as they do in a cluster.
//
// What envtest does not have is kube-controller-manager. Nothing garbage
// collects and nothing finishes the deletion of a namespace. The tests use
// that: a deleted namespace stays in Terminating until the test itself plays
// the namespace controller (see finalizeNamespace), so both halves of the
// finalizer's promise can be checked separately.

var (
	cfg       *rest.Config
	k8sClient client.Client
	testEnv   *envtest.Environment
	ctx       context.Context
	cancel    context.CancelFunc

	// operatorWrites sees every write the operator makes.
	operatorWrites *recordingClient
)

func TestControllers(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "ProjectClaim operator")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))
	SetDefaultEventuallyTimeout(20 * time.Second)
	SetDefaultEventuallyPollingInterval(100 * time.Millisecond)
	SetDefaultConsistentlyDuration(2 * time.Second)
	SetDefaultConsistentlyPollingInterval(100 * time.Millisecond)

	ctx, cancel = context.WithCancel(context.Background())

	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	var err error
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())

	utilruntime.Must(platformv1alpha1.AddToScheme(clientgoscheme.Scheme))

	// The test client talks to the API server directly, so assertions never
	// read the operator's cache.
	k8sClient, err = client.New(cfg, client.Options{Scheme: clientgoscheme.Scheme})
	Expect(err).NotTo(HaveOccurred())

	applyFile(filepath.Join("..", "..", "config", "admission", "tenant-ownership.yaml"))
	allowClaimAuthorsToWriteClaims()
	fillBuiltInRoles()

	mgr, err := ctrl.NewManager(operatorConfig(), ctrl.Options{
		Scheme:  clientgoscheme.Scheme,
		Cache:   CacheOptions(),
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	Expect(err).NotTo(HaveOccurred())
	Expect(SetupIndexes(ctx, mgr)).To(Succeed())

	operatorWrites = &recordingClient{Client: mgr.GetClient()}

	Expect((&ProjectClaimReconciler{
		Client:    operatorWrites,
		APIReader: mgr.GetAPIReader(),
		Scheme:    mgr.GetScheme(),
		Recorder:  mgr.GetEventRecorderFor(ManagerName),
		Clock:     clock.RealClock{},
		// Several workers on purpose. The budget test relies on claims
		// really being reconciled in parallel.
		MaxConcurrentReconciles: 4,
	}).SetupWithManager(mgr)).To(Succeed())

	Expect((&TenantReconciler{
		Client:    operatorWrites,
		APIReader: mgr.GetAPIReader(),
	}).SetupWithManager(mgr)).To(Succeed())

	go func() {
		defer GinkgoRecover()
		Expect(mgr.Start(ctx)).To(Succeed())
	}()
})

var _ = AfterSuite(func() {
	cancel()
	Expect(testEnv.Stop()).To(Succeed())
})

// operatorUser is the identity the operator's ServiceAccount has in a cluster.
const operatorUser = "system:serviceaccount:projectclaim-system:projectclaim-controller-manager"

// operatorConfig returns credentials for a user that holds the ClusterRole in
// config/rbac/role.yaml and nothing else.
//
// Running the operator as cluster admin, which is what a test suite does by
// default, hides every missing permission: the tests pass and the first real
// deployment fails with "forbidden". This way a rule missing from the
// generated role fails the suite. It also exercises two things that only
// matter for a non-admin identity: the bind verb that lets the operator hand
// out roles it does not hold, and the administer verb the admission policy
// checks before letting it edit claims of any tenant.
func operatorConfig() *rest.Config {
	applyFile(filepath.Join("..", "..", "config", "rbac", "role.yaml"))
	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "test-operator"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "projectclaim-manager"},
		Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: operatorUser}},
	}
	Expect(k8sClient.Create(ctx, binding)).To(Succeed())

	user, err := testEnv.AddUser(envtest.User{Name: operatorUser}, nil)
	Expect(err).NotTo(HaveOccurred())
	return user.Config()
}

// applyFile creates every object in a manifest, as shipped, so the tests run
// against the same policy and the same role a cluster would get.
func applyFile(path string) {
	f, err := os.Open(path)
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = f.Close() }()

	decoder := utilyaml.NewYAMLOrJSONDecoder(f, 4096)
	for {
		obj := &unstructured.Unstructured{}
		err := decoder.Decode(obj)
		if errors.Is(err, io.EOF) {
			break
		}
		Expect(err).NotTo(HaveOccurred())
		if len(obj.Object) == 0 {
			continue
		}
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
	}
}

// fillBuiltInRoles gives the admin, edit and view ClusterRoles some content.
//
// In a cluster they are filled by the role aggregation controller, which is
// part of kube-controller-manager and so missing here. Left empty, binding
// someone to them escalates nothing, and the API server would let the
// operator create those bindings even without the bind verb. With rules in
// them that the operator does not hold itself, the escalation check applies
// as it does in a real cluster.
func fillBuiltInRoles() {
	for _, name := range []string{"admin", "edit", "view"} {
		Eventually(func() error {
			role := &rbacv1.ClusterRole{}
			if err := k8sClient.Get(ctx, client.ObjectKey{Name: name}, role); err != nil {
				return err
			}
			role.Rules = []rbacv1.PolicyRule{{
				APIGroups: []string{""},
				Resources: []string{"pods", "configmaps"},
				Verbs:     []string{"get", "list", "watch"},
			}}
			return k8sClient.Update(ctx, role)
		}).Should(Succeed())
	}
}

// claimAuthors is the group the impersonated test users are in.
const claimAuthors = "claim-authors"

// allowClaimAuthorsToWriteClaims gives that group plain RBAC access to
// claims. Whether a given user may write a given claim is then entirely up to
// the admission policy, which is what the ownership tests want to isolate.
func allowClaimAuthorsToWriteClaims() {
	role := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim-author"},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{platformv1alpha1.GroupVersion.Group},
			Resources: []string{"projectclaims"},
			Verbs:     []string{"get", "list", "create", "update", "patch", "delete"},
		}},
	}
	Expect(k8sClient.Create(ctx, role)).To(Succeed())
	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim-author"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
		Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.GroupKind, Name: claimAuthors}},
	}
	Expect(k8sClient.Create(ctx, binding)).To(Succeed())
}
