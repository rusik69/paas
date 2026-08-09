//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rusik69/paas/pkg/wait"
)

// TestCluster_AllNodesReady is the cheapest possible smoke test and runs first
// by name ordering, so a broken cluster fails in seconds rather than after the
// storage suite has spent ten minutes timing out.
func TestCluster_AllNodesReady(t *testing.T) {
	want := len(topology(t))

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	var last string
	err := wait.For(ctx, 5*time.Second, "all nodes Ready", func(ctx context.Context) (bool, error) {
		nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			last = err.Error()
			return false, nil
		}
		ready := 0
		for _, n := range nodes.Items {
			for _, c := range n.Status.Conditions {
				if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
					ready++
				}
			}
		}
		last = ""
		return ready == want, nil
	})
	if err != nil {
		t.Fatalf("%v (last client error: %q)", err, last)
	}
}

// TestCluster_KubeProxyIsAbsent guards ADR 0002. Cilium's kube-proxy
// replacement and a real kube-proxy both program service load balancing, and
// when both are present the resulting behaviour depends on rule ordering — so
// this asserts the specific absence rather than that services happen to work.
func TestCluster_KubeProxyIsAbsent(t *testing.T) {
	pods, err := clientset.CoreV1().Pods("kube-system").List(t.Context(), metav1.ListOptions{
		LabelSelector: "k8s-app=kube-proxy",
	})
	if err != nil {
		t.Fatalf("list kube-system pods: %v", err)
	}
	if len(pods.Items) != 0 {
		t.Errorf("found %d kube-proxy pods, want 0 — Talos cluster.proxy.disabled is not in effect",
			len(pods.Items))
	}
}

// TestCluster_CiliumRunsOnEveryNode catches the case where Cilium installs but
// its DaemonSet cannot schedule on a tainted node, which presents later as pods
// on that one node having no network.
func TestCluster_CiliumRunsOnEveryNode(t *testing.T) {
	ds, err := clientset.AppsV1().DaemonSets("kube-system").Get(t.Context(), "cilium", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get daemonset/cilium: %v", err)
	}
	if got, want := ds.Status.NumberAvailable, ds.Status.DesiredNumberScheduled; got != want {
		t.Errorf("cilium available = %d, want %d (desired scheduled)", got, want)
	}
	if got, want := int(ds.Status.DesiredNumberScheduled), len(topology(t)); got != want {
		t.Errorf("cilium desired = %d, want %d — one per node", got, want)
	}
}

// TestCluster_ReplicatedStorageClassesExist asserts the contract the rest of the
// platform is written against: replicated-3 is the default, and the scratch
// class is labelled non-durable so the phase-3 catalog can refuse to offer it
// for databases.
func TestCluster_ReplicatedStorageClassesExist(t *testing.T) {
	classes, err := clientset.StorageV1().StorageClasses().List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list storageclasses: %v", err)
	}

	byName := map[string]bool{}
	defaults := []string{}
	durability := map[string]string{}
	for _, sc := range classes.Items {
		byName[sc.Name] = true
		durability[sc.Name] = sc.Annotations["paas.io/durability"]
		if sc.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" {
			defaults = append(defaults, sc.Name)
		}
	}

	for _, want := range []string{"replicated-2", "replicated-3", "scratch"} {
		if !byName[want] {
			t.Errorf("storageclass %q is missing", want)
		}
	}
	if len(defaults) != 1 || defaults[0] != "replicated-3" {
		t.Errorf("default storageclass = %v, want exactly [replicated-3]", defaults)
	}
	if got := durability["scratch"]; got != "non-durable" {
		t.Errorf("scratch paas.io/durability = %q, want %q", got, "non-durable")
	}
}
