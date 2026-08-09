//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rusik69/paas/pkg/wait"
)

const (
	registryNamespace = "paas-system"
	registryService   = "registry.paas-system.svc.cluster.local:5000"
	// The name containerd resolves through the Talos mirror, not a DNS name.
	registryMirrorHost = "registry.paas.io"
)

func TestRegistry_IsBackedByReplicatedStorage(t *testing.T) {
	pvc, err := clientset.CoreV1().PersistentVolumeClaims(registryNamespace).
		Get(t.Context(), "registry-data", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pvc registry-data: %v", err)
	}
	if pvc.Status.Phase != corev1.ClaimBound {
		t.Errorf("registry-data phase = %s, want %s", pvc.Status.Phase, corev1.ClaimBound)
	}
	// Chart artifacts and app images are not reconstructible from anywhere
	// else, so the registry must not sit on the scratch class.
	got := "<none>"
	if pvc.Spec.StorageClassName != nil {
		got = *pvc.Spec.StorageClassName
	}
	if got != "replicated-3" {
		t.Errorf("registry-data storageClassName = %q, want %q", got, "replicated-3")
	}
}

// Both the containerd mirror and the socket load balancer are per-node
// configuration, so one scheduled pod proves only whichever node the scheduler
// happened to pick — and a mirror missing from a single node's machine config
// stays invisible until something schedules there.
//
// Pinned by nodeName rather than scheduled, tolerating everything: the control
// plane carries a NoSchedule taint (allowSchedulingOnControlPlanes: false) and
// its containerd pulls images just the same.
func runOnEveryNode(t *testing.T, ns, name string, spec corev1.PodSpec) {
	t.Helper()

	nodes, err := clientset.CoreV1().Nodes().List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	if len(nodes.Items) == 0 {
		t.Fatal("cluster has no nodes")
	}

	for _, n := range nodes.Items {
		pinned := spec
		pinned.NodeName = n.Name
		pinned.RestartPolicy = corev1.RestartPolicyNever
		pinned.Tolerations = []corev1.Toleration{{Operator: corev1.TolerationOpExists}}
		createPod(t, ns, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-" + n.Name},
			Spec:       pinned,
		})
	}

	// Named, so the log shows which nodes the run actually covered rather than
	// leaving "every node" to be taken on trust.
	t.Logf("%s on %d nodes: %s", name, len(nodes.Items), strings.Join(nodeNames(nodes.Items), " "))

	for _, n := range nodes.Items {
		pod := name + "-" + n.Name
		phase, err := waitPodTerminated(t, ns, pod, 5*time.Minute)
		if err != nil {
			t.Errorf("node %s: pod %s did not terminate: %v", n.Name, pod, err)
			continue
		}
		if phase != corev1.PodSucceeded {
			t.Errorf("node %s: pod %s phase = %s, want %s", n.Name, pod, phase, corev1.PodSucceeded)
		}
	}
}

func nodeNames(nodes []corev1.Node) []string {
	names := make([]string, len(nodes))
	for i, n := range nodes {
		names[i] = n.Name
	}
	return names
}

func registryClusterIP(t *testing.T) string {
	t.Helper()

	svc, err := clientset.CoreV1().Services(registryNamespace).Get(t.Context(), "registry", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get service registry: %v", err)
	}
	if svc.Spec.ClusterIP == "" {
		t.Fatal("service registry has no clusterIP")
	}
	return svc.Spec.ClusterIP
}

// hack/manifests/registry.yaml pins the Service's clusterIP and
// hack/talos/common.patch.yaml independently pins the same address as the
// containerd mirror endpoint. talosctl apply-config converges the Talos side
// even when the two have drifted, while re-applying the Service is rejected
// outright because clusterIP is immutable — so a disagreement is silent
// unless something reads both sides and compares them, which is what this
// test does.
func TestRegistry_MirrorEndpointMatchesServiceClusterIP(t *testing.T) {
	clusterIP := registryClusterIP(t)

	for _, n := range topology(t) {
		out, err := exec.CommandContext(t.Context(), "talosctl",
			"get", "machineconfig", "-o", "yaml", "--nodes", n.IP).CombinedOutput()
		if err != nil {
			t.Errorf("talosctl get machineconfig on %s: %v\n%s", n.Domain, err, out)
			continue
		}

		// talosctl returns the whole machine config as one escaped YAML scalar
		// under spec:, so its newlines arrive as literal backslash-n.
		// Unescaping makes it line-oriented, which is what keeps the lookup
		// anchored inside this mirror's own block once a second mirror exists.
		endpoint, ok := mirrorEndpoint(strings.ReplaceAll(string(out), `\n`, "\n"), registryMirrorHost)
		if !ok {
			t.Errorf("node %s machine config has no mirror endpoint for %s", n.Domain, registryMirrorHost)
			continue
		}
		if !strings.Contains(endpoint, clusterIP) {
			t.Errorf("node %s mirror endpoint for %s = %q, want it to contain the registry Service clusterIP %s",
				n.Domain, registryMirrorHost, endpoint, clusterIP)
		}
	}
}

// The first list item nested under `<host>:`, which for a registry mirror is
// its endpoint. Bounded by indentation so a mirror configured with no endpoints
// reports as absent rather than borrowing the next mirror's.
func mirrorEndpoint(cfg, host string) (string, bool) {
	lines := strings.Split(cfg, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) != host+":" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		for _, next := range lines[i+1:] {
			trimmed := strings.TrimSpace(next)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			if len(next)-len(strings.TrimLeft(next, " ")) <= indent {
				break
			}
			if endpoint, found := strings.CutPrefix(trimmed, "- "); found {
				return endpoint, true
			}
		}
	}
	return "", false
}

// containerd runs in the Talos host network namespace and reaches the
// registry Service by ClusterIP, which only works if Cilium's socket load
// balancer is programming that namespace — there is no kube-proxy. This test
// takes exactly that path without touching image pulls at all: a failure here
// means the socket LB is broken, and a pass here alongside a failing mirror
// pull means the fault is in the mirror configuration, not the dataplane.
func TestRegistry_ClusterIPReachableFromHostNetwork(t *testing.T) {
	clusterIP := registryClusterIP(t)

	ns := namespace(t, "e2e-registry-hostnet")
	t.Cleanup(func() {
		if t.Failed() {
			dumpNamespace(t, ns)
		}
	})

	runOnEveryNode(t, ns, "fetcher", corev1.PodSpec{
		HostNetwork: true,
		Containers: []corev1.Container{{
			Name:  "main",
			Image: *busyboxImage,
			// wget's own exit status carries the result: non-zero on any
			// connection failure, which is what turns an unreachable
			// ClusterIP into a Failed pod rather than a silent success.
			Command: []string{"wget", "-q", "-O", "/dev/null", fmt.Sprintf("http://%s:5000/v2/", clusterIP)},
		}},
	})
}

// The end-to-end proof: an image pushed into the cluster registry is pullable
// by a node through the Talos mirror. Asserting only that the Deployment is
// Available would pass against a registry nothing can reach.
func TestRegistry_PushedImageIsPullableThroughTheTalosMirror(t *testing.T) {
	ns := namespace(t, "e2e-registry")
	t.Cleanup(func() {
		if t.Failed() {
			dumpNamespace(t, ns)
		}
	})

	ref := "e2e/busybox:" + fmt.Sprint(time.Now().UnixNano())
	crane := "gcr.io/go-containerregistry/crane:" + pinnedVersion(t, "CRANE_VERSION")

	// 0, not the default: a retry would let a registry that only serves one
	// push in three pass on its second or third attempt, hiding the
	// intermittency this test exists to catch.
	backoffLimit := int32(0)
	push := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "push"},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:  "crane",
						Image: crane,
						// --insecure: the registry speaks plain HTTP, which is
						// why no CA has to be distributed to every node.
						Args: []string{"copy", *busyboxImage, registryService + "/" + ref, "--insecure"},
					}},
				},
			},
		},
	}
	if _, err := clientset.BatchV1().Jobs(ns).Create(t.Context(), push, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create push job: %v", err)
	}
	if err := waitJobSucceeded(t, ns, "push", 10*time.Minute); err != nil {
		t.Fatalf("pushing %s into the cluster registry failed: %v", ref, err)
	}

	// Pulled by mirror host, so success means containerd resolved
	// registry.paas.io through machine.registries and reached the Service IP
	// from the host namespace. The tag is unique per run, so no node can pass
	// on a layer another node already cached.
	runOnEveryNode(t, ns, "puller", corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:    "main",
			Image:   registryMirrorHost + "/" + ref,
			Command: []string{"true"},
		}},
	})
}

func waitJobSucceeded(t *testing.T, ns, name string, timeout time.Duration) error {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()

	return wait.For(ctx, 5*time.Second, "job "+ns+"/"+name+" to succeed", func(ctx context.Context) (bool, error) {
		job, err := clientset.BatchV1().Jobs(ns).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		for _, c := range job.Status.Conditions {
			if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
				return false, fmt.Errorf("job failed: %s: %s", c.Reason, c.Message)
			}
		}
		return job.Status.Succeeded > 0, nil
	})
}
