package kubernetes

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ListResourcePage passes the source query to the existing checked clientset.
// Only built-in, nonsensitive resource types can be listed through this path.
func (c *Client) ListResourcePage(ctx context.Context, resource, ns string, opts metav1.ListOptions) (runtime.Object, error) {
	switch resource {
	case "/pods":
		return c.clientset.CoreV1().Pods(ns).List(ctx, opts)
	case "/nodes":
		return c.clientset.CoreV1().Nodes().List(ctx, opts)
	case "apps/deployments":
		return c.clientset.AppsV1().Deployments(ns).List(ctx, opts)
	case "apps/statefulsets":
		return c.clientset.AppsV1().StatefulSets(ns).List(ctx, opts)
	case "apps/daemonsets":
		return c.clientset.AppsV1().DaemonSets(ns).List(ctx, opts)
	case "apps/replicasets":
		return c.clientset.AppsV1().ReplicaSets(ns).List(ctx, opts)
	case "/services":
		return c.clientset.CoreV1().Services(ns).List(ctx, opts)
	case "/endpoints":
		return c.clientset.CoreV1().Endpoints(ns).List(ctx, opts)
	case "discovery.k8s.io/endpointslices":
		return c.clientset.DiscoveryV1().EndpointSlices(ns).List(ctx, opts)
	case "/persistentvolumeclaims":
		return c.clientset.CoreV1().PersistentVolumeClaims(ns).List(ctx, opts)
	case "/persistentvolumes":
		return c.clientset.CoreV1().PersistentVolumes().List(ctx, opts)
	case "storage.k8s.io/storageclasses":
		return c.clientset.StorageV1().StorageClasses().List(ctx, opts)
	case "/namespaces":
		return c.clientset.CoreV1().Namespaces().List(ctx, opts)
	case "/configmaps":
		return c.clientset.CoreV1().ConfigMaps(ns).List(ctx, opts)
	case "/events":
		return c.clientset.CoreV1().Events(ns).List(ctx, opts)
	default:
		return nil, fmt.Errorf("unsupported resource %q", resource)
	}
}
