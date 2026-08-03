//go:build e2e

package framework

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sandboxv1alpha1 "github.com/AgentNaut/SandboxFleet/api/v1alpha1"
	sandboxfleet "github.com/AgentNaut/SandboxFleet/clients/go/sandboxfleet"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const workerAPIPort = 8090

// Context is the host-side e2e harness (same model as agent-sandbox:
// go test on the developer machine, talking to kind via kubeconfig).
type Context struct {
	T          *testing.T
	RestConfig *rest.Config
	K8s        client.Client
	SDK        sandboxfleet.Client
}

func New(t *testing.T) *Context {
	t.Helper()
	restConfig, err := loadRESTConfig()
	if err != nil {
		t.Fatalf("load kubeconfig: %v", err)
	}
	k8s, err := newKubernetesClient(restConfig)
	if err != nil {
		t.Fatalf("create kubernetes client: %v", err)
	}
	sdk, err := sandboxfleet.New(restConfig)
	if err != nil {
		t.Fatalf("create SDK client: %v", err)
	}
	return &Context{T: t, RestConfig: restConfig, K8s: k8s, SDK: sdk}
}

func (c *Context) CreateNamespace(ctx context.Context, name string) {
	c.T.Helper()
	_ = c.K8s.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})
	_ = wait.PollUntilContextTimeout(ctx, time.Second, 90*time.Second, true, func(ctx context.Context) (bool, error) {
		err := c.K8s.Get(ctx, types.NamespacedName{Name: name}, &corev1.Namespace{})
		if err == nil {
			return false, nil
		}
		return client.IgnoreNotFound(err) == nil, nil
	})
	if err := c.K8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
		c.T.Fatalf("create namespace %q: %v", name, err)
	}
	c.T.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = c.K8s.Delete(cleanupCtx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})
	})
}

func (c *Context) CreatePool(ctx context.Context, namespace, name string) {
	c.T.Helper()
	root, err := repoRoot()
	if err != nil {
		c.T.Fatalf("find repo root: %v", err)
	}
	c.ApplyManifest(ctx, filepath.Join(root, "test", "e2e", "testdata", "sandboxpool.yaml"), map[string]string{
		"NAMESPACE":       namespace,
		"NAME":            name,
		"RUNTIME_HANDLER": runtimeHandler(),
	})
}

// ApplyManifest reads a YAML file, substitutes {{KEY}} placeholders, and creates objects.
func (c *Context) ApplyManifest(ctx context.Context, path string, vars map[string]string) {
	c.T.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		c.T.Fatalf("read manifest %q: %v", path, err)
	}
	rendered := string(raw)
	for key, value := range vars {
		rendered = strings.ReplaceAll(rendered, "{{"+key+"}}", value)
	}

	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader([]byte(rendered)), 4096)
	for {
		var obj unstructured.Unstructured
		if err := decoder.Decode(&obj); err != nil {
			if err == io.EOF {
				break
			}
			c.T.Fatalf("decode manifest %q: %v", path, err)
		}
		if len(obj.Object) == 0 {
			continue
		}
		if err := c.K8s.Create(ctx, &obj); err != nil {
			c.T.Fatalf("create from manifest %q (%s/%s): %v", path, obj.GetKind(), obj.GetName(), err)
		}
	}
}

func (c *Context) WaitPoolReady(ctx context.Context, namespace, name string) {
	c.T.Helper()
	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 8*time.Minute, true, func(ctx context.Context) (bool, error) {
		var pool sandboxv1alpha1.SandboxPool
		if err := c.K8s.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &pool); err != nil {
			return false, err
		}
		ready := meta.FindStatusCondition(pool.Status.Conditions, sandboxv1alpha1.ConditionReady)
		return ready != nil && ready.Status == metav1.ConditionTrue && pool.Status.ReadyWorkers > 0, nil
	})
	if err != nil {
		c.T.Fatalf("wait SandboxPool Ready: %v", err)
	}
}

// ExecSandbox runs a command in the assigned Worker. Worker selection comes from
// Sandbox status; port-forward is only used so the host process can reach Pod IP.
func (c *Context) ExecSandbox(ctx context.Context, sandbox *sandboxv1alpha1.Sandbox, command []string) *sandboxfleet.ExecResult {
	c.T.Helper()
	if sandbox.Status.Assignment == nil || sandbox.Status.Assignment.Worker == "" {
		c.T.Fatalf("Sandbox has no Worker assignment")
	}
	endpoint, stop, err := portForward(ctx, c.RestConfig, sandbox.Namespace, sandbox.Status.Assignment.Worker, workerAPIPort)
	if err != nil {
		c.T.Fatalf("reach Worker API: %v", err)
	}
	defer stop()

	result, err := c.SDK.ExecSandbox(ctx, sandbox.Namespace, sandbox.Name, sandboxfleet.ExecOptions{
		Command:        command,
		Timeout:        30 * time.Second,
		WorkerEndpoint: endpoint,
	})
	if err != nil {
		c.T.Fatalf("ExecSandbox: %v", err)
	}
	return result
}

func loadRESTConfig() (*rest.Config, error) {
	if kubeconfig := os.Getenv("KUBECONFIG"); kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	if _, err := os.Stat("bin/KUBECONFIG"); err == nil {
		return clientcmd.BuildConfigFromFlags("", "bin/KUBECONFIG")
	}
	if root, err := repoRoot(); err == nil {
		path := filepath.Join(root, "bin", "KUBECONFIG")
		if _, err := os.Stat(path); err == nil {
			return clientcmd.BuildConfigFromFlags("", path)
		}
	}
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{}
	if contextName := os.Getenv("E2E_KUBE_CONTEXT"); contextName != "" {
		overrides.CurrentContext = contextName
	} else {
		overrides.CurrentContext = "kind-sandboxfleet"
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
}

func repoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir := wd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found from %s", wd)
		}
		dir = parent
	}
}

func newKubernetesClient(config *rest.Config) (client.Client, error) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := sandboxv1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	return client.New(config, client.Options{Scheme: scheme})
}

func portForward(ctx context.Context, config *rest.Config, namespace, podName string, remotePort int) (string, func(), error) {
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return "", nil, err
	}
	reqURL := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(namespace).
		Name(podName).
		SubResource("portforward").
		URL()
	transport, upgrader, err := spdy.RoundTripperFor(config)
	if err != nil {
		return "", nil, err
	}
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, reqURL)
	readyCh := make(chan struct{})
	stopCh := make(chan struct{})
	forwarder, err := portforward.New(dialer, []string{fmt.Sprintf("0:%d", remotePort)}, stopCh, readyCh, io.Discard, io.Discard)
	if err != nil {
		return "", nil, err
	}
	errCh := make(chan error, 1)
	go func() { errCh <- forwarder.ForwardPorts() }()
	select {
	case <-ctx.Done():
		close(stopCh)
		return "", nil, ctx.Err()
	case err := <-errCh:
		return "", nil, fmt.Errorf("port-forward failed: %w", err)
	case <-readyCh:
	}
	ports, err := forwarder.GetPorts()
	if err != nil {
		close(stopCh)
		return "", nil, err
	}
	if len(ports) == 0 {
		close(stopCh)
		return "", nil, fmt.Errorf("port-forward returned no ports")
	}
	return fmt.Sprintf("http://127.0.0.1:%d", ports[0].Local), func() { close(stopCh) }, nil
}

func runtimeHandler() string {
	if handler := os.Getenv("E2E_RUNTIME_HANDLER"); handler != "" {
		return handler
	}
	return "runsc"
}
