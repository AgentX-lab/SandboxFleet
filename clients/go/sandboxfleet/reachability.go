package sandboxfleet

import (
	"context"
	"fmt"
	"net"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// WorkerReachability resolves how the SDK reaches a Worker HTTP API.
// Default is direct Pod IP; e2e can inject port-forward without changing call sites.
type WorkerReachability interface {
	ReachWorker(ctx context.Context, namespace, workerName string, port int32) (endpoint string, cleanup func(), err error)
}

type directPodIPReachability struct {
	client client.Client
}

func (d *directPodIPReachability) ReachWorker(ctx context.Context, namespace, workerName string, port int32) (string, func(), error) {
	var pod corev1.Pod
	if err := d.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: workerName}, &pod); err != nil {
		return "", nil, fmt.Errorf("get Worker Pod %q: %w", workerName, err)
	}
	if pod.Status.PodIP == "" {
		return "", nil, fmt.Errorf("Worker Pod %q has no IP", workerName)
	}
	endpoint := "http://" + net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(int(port)))
	return endpoint, func() {}, nil
}
