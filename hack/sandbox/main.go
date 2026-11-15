// Command sandbox starts a throwaway Kubernetes API server and etcd on this
// machine, installs the CRDs and the admission policy, and writes a
// kubeconfig. It is the quickest way to try the operator: no cluster, no
// container runtime, nothing to clean up afterwards.
//
// It is the same control plane the test suite runs against (envtest), so it
// has the same limit: there is no kube-controller-manager. Pods are never
// scheduled, and a deleted namespace stays in Terminating. For this operator
// that is a convenient way to watch the finalizer hold a claim.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func main() {
	// Not called --kubeconfig: controller-runtime already registers a flag of
	// that name, for reading a kubeconfig rather than writing one.
	out := flag.String("out", "bin/sandbox.kubeconfig", "Where to write the kubeconfig.")
	flag.Parse()

	if err := run(*out); err != nil {
		fmt.Fprintln(os.Stderr, "sandbox:", err)
		os.Exit(1)
	}
}

func run(kubeconfigPath string) error {
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		return fmt.Errorf("starting the API server (is KUBEBUILDER_ASSETS set? try `make sandbox`): %w", err)
	}
	defer func() { _ = env.Stop() }()

	c, err := client.New(cfg, client.Options{})
	if err != nil {
		return err
	}
	if err := apply(c, filepath.Join("config", "admission", "tenant-ownership.yaml")); err != nil {
		return fmt.Errorf("installing the admission policy: %w", err)
	}

	admin, err := env.AddUser(envtest.User{Name: "sandbox-admin", Groups: []string{"system:masters"}}, nil)
	if err != nil {
		return err
	}
	data, err := admin.KubeConfig()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(kubeconfigPath), 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(kubeconfigPath, data, 0o600); err != nil {
		return err
	}

	fmt.Printf("API server is up at %s\n\n", cfg.Host)
	fmt.Printf("In another terminal:\n\n")
	fmt.Printf("  export KUBECONFIG=%s\n", kubeconfigPath)
	fmt.Printf("  go run ./cmd --metrics-secure=false --metrics-bind-address=:8080 &\n")
	fmt.Printf("  kubectl apply -k config/samples\n")
	fmt.Printf("  kubectl get projectclaims,tenants,projecttiers\n\n")
	fmt.Printf("Ctrl-C here stops the API server and throws its data away.\n")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	return nil
}

func apply(c client.Client, path string) error {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	decoder := utilyaml.NewYAMLOrJSONDecoder(f, 4096)
	for {
		obj := &unstructured.Unstructured{}
		err := decoder.Decode(obj)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if len(obj.Object) == 0 {
			continue
		}
		if err := c.Create(context.Background(), obj); err != nil {
			return err
		}
	}
}
