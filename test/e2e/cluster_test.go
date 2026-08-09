//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rusik69/paas/pkg/wait"
)

// First by name ordering, so a broken cluster fails in seconds rather than
// after the storage suite has spent ten minutes timing out.
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

// Pointing KUBECONFIG at kind or minikube must fail here rather than pass
// everywhere below: none of the phase-0 assertions mean anything off Talos.
func TestCluster_NodesAreRealTalosGuests(t *testing.T) {
	wantVersion := pinnedVersion(t, "TALOS_VERSION")

	nodes, err := clientset.CoreV1().Nodes().List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	if len(nodes.Items) == 0 {
		t.Fatal("cluster has no nodes")
	}

	for _, n := range nodes.Items {
		os := n.Status.NodeInfo.OSImage
		if !strings.Contains(os, "Talos") {
			t.Errorf("node %s osImage = %q, want Talos — e2e must run on real Talos guests", n.Name, os)
			continue
		}
		if !strings.Contains(os, wantVersion) {
			t.Errorf("node %s osImage = %q, want Talos %s as pinned in hack/versions.sh",
				n.Name, os, wantVersion)
		}
	}
}

// Absent, this surfaces ten minutes later as a PVC stuck in Pending with a
// mount error that never mentions DRBD.
func TestCluster_DRBDExtensionIsInstalled(t *testing.T) {
	for _, n := range topology(t) {
		out, err := exec.CommandContext(t.Context(), "talosctl",
			"get", "extensions", "--nodes", n.IP).CombinedOutput()
		if err != nil {
			t.Errorf("talosctl get extensions on %s: %v\n%s", n.Domain, err, out)
			continue
		}
		if !strings.Contains(string(out), "drbd") {
			t.Errorf("node %s carries no drbd system extension:\n%s", n.Domain, out)
		}
	}
}

// Guards ADR 0002. Cilium's kube-proxy replacement and a real kube-proxy both
// program service load balancing, and with both present the behaviour depends
// on rule ordering — so this asserts the absence rather than that services
// happen to work.
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

// Catches Cilium installing but failing to schedule on a tainted node, which
// presents later as pods on that one node having no network.
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

// Guards GATEWAY_API_VERSION. Cilium understands only the CRD version it
// declares conformance against, and a newer one is accepted by the apiserver
// and then surfaces as a reconcile error on the GatewayClass — which, without
// this, nothing would notice until phase 4 built routing on top of it.
func TestCluster_GatewayClassIsAccepted(t *testing.T) {
	gvr := schema.GroupVersionResource{
		Group:    "gateway.networking.k8s.io",
		Version:  "v1",
		Resource: "gatewayclasses",
	}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	var last string
	err := wait.For(ctx, 5*time.Second, "gatewayclass/cilium Accepted", func(ctx context.Context) (bool, error) {
		gc, err := dynClient.Resource(gvr).Get(ctx, "cilium", metav1.GetOptions{})
		if err != nil {
			// Once the deadline passes every call fails the same way, and
			// recording that would overwrite the reconcile message this test
			// exists to report.
			if ctx.Err() == nil {
				last = err.Error()
			}
			return false, nil
		}
		conds, _, err := unstructured.NestedSlice(gc.Object, "status", "conditions")
		if err != nil {
			return false, err
		}
		for _, c := range conds {
			m, ok := c.(map[string]any)
			if !ok || m["type"] != "Accepted" {
				continue
			}
			last = fmt.Sprintf("status=%v reason=%v message=%v", m["status"], m["reason"], m["message"])
			return m["status"] == string(metav1.ConditionTrue), nil
		}
		last = "no Accepted condition on gatewayclass/cilium"
		return false, nil
	})
	if err != nil {
		t.Fatalf("%v (last: %s)", err, last)
	}
}

// The contract the rest of the platform is written against.
func TestCluster_ReplicatedStorageClassesExist(t *testing.T) {
	classes, err := clientset.StorageV1().StorageClasses().List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list storageclasses: %v", err)
	}

	byName := map[string]bool{}
	durability := map[string]string{}
	placement := map[string]string{}
	var defaults []string
	for _, sc := range classes.Items {
		byName[sc.Name] = true
		durability[sc.Name] = sc.Annotations["paas.io/durability"]
		placement[sc.Name] = sc.Parameters["linstor.csi.linbit.com/placementCount"]
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

	// The number in the name is the contract every later phase is written
	// against. Editing the parameter without renaming the class downgrades
	// every volume it provisions, and the only visible symptom is data loss
	// on a node failure the class promised to survive.
	for name, want := range map[string]string{"replicated-2": "2", "replicated-3": "3"} {
		if got := placement[name]; got != want {
			t.Errorf("storageclass %s placementCount = %q, want %q", name, got, want)
		}
	}
}
