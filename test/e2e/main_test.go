//go:build e2e

// Package e2e holds every assertion made against a real cluster.
//
// hack/e2e.sh provisions and asserts nothing; this package asserts and
// provisions nothing beyond Kubernetes objects. The split is what stops the
// bash growing a second, untested test framework inside it.
package e2e

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	kubeconfig = flag.String("kubeconfig", os.Getenv("KUBECONFIG"),
		"path to the e2e cluster kubeconfig; defaults to $KUBECONFIG")
	e2eScript = flag.String("e2e-script", "../../hack/e2e.sh",
		"path to the provisioning script, used for node power control")
	versionsScript = flag.String("versions-script", "../../hack/versions.sh",
		"path to the pinned-version definitions")
	busyboxImage = flag.String("busybox-image", "busybox:1.36",
		"image used by storage fixtures")

	clientset *kubernetes.Clientset
)

func TestMain(m *testing.M) {
	flag.Parse()

	if *kubeconfig == "" {
		fmt.Fprintln(os.Stderr, "e2e: no kubeconfig; run 'make cluster-up' first")
		os.Exit(1)
	}

	cfg, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: load kubeconfig %s: %v\n", *kubeconfig, err)
		os.Exit(1)
	}
	// The suite kills nodes on purpose. Unbounded, a call to a dead endpoint
	// blocks until the test's own deadline and the failure names the wrong
	// thing.
	cfg.Timeout = 30 * time.Second

	if clientset, err = kubernetes.NewForConfig(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: build client: %v\n", err)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

type node struct {
	Domain string // libvirt domain
	IP     string
}

// Read from the provisioning script rather than restated, so the two can never
// disagree about which guest is which.
func topology(t *testing.T) []node {
	t.Helper()

	out, err := exec.CommandContext(t.Context(), *e2eScript, "nodes").Output()
	if err != nil {
		t.Fatalf("e2e.sh nodes: %v", err)
	}

	var nodes []node
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 3 {
			t.Fatalf("e2e.sh nodes: malformed line %q", line)
		}
		nodes = append(nodes, node{Domain: fields[0], IP: fields[2]})
	}
	if len(nodes) < 3 {
		t.Fatalf("topology has %d nodes, want at least 3 — DRBD replication needs three", len(nodes))
	}
	return nodes
}

// Read from hack/versions.sh, or the test can agree with itself while
// disagreeing with the cluster.
func pinnedVersion(t *testing.T, name string) string {
	t.Helper()

	script := fmt.Sprintf("source %q; printf '%%s' \"${%s}\"", *versionsScript, name)
	out, err := exec.CommandContext(t.Context(), "bash", "-c", script).Output()
	if err != nil {
		t.Fatalf("read %s from %s: %v", name, *versionsScript, err)
	}
	if len(out) == 0 {
		t.Fatalf("%s is empty in %s", name, *versionsScript)
	}
	return string(out)
}

// powerOff registers the restart with t.Cleanup, so a failing assertion still
// leaves the cluster usable for the next test.
func powerOff(t *testing.T, domain string) {
	t.Helper()

	if out, err := exec.CommandContext(t.Context(), *e2eScript, "kill-node", domain).CombinedOutput(); err != nil {
		t.Fatalf("kill-node %s: %v\n%s", domain, err, out)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if out, err := exec.CommandContext(ctx, *e2eScript, "start-node", domain).CombinedOutput(); err != nil &&
			!strings.Contains(string(out), "already active") {
			t.Logf("cleanup: restarting %s failed: %v\n%s", domain, err, out)
		}
	})
}

func powerOn(t *testing.T, domain string) {
	t.Helper()

	out, err := exec.CommandContext(t.Context(), *e2eScript, "start-node", domain).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "already active") {
		t.Fatalf("start-node %s: %v\n%s", domain, err, out)
	}
}

func namespace(t *testing.T, prefix string) string {
	t.Helper()

	ns, err := clientset.CoreV1().Namespaces().Create(t.Context(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: prefix + "-",
			Labels: map[string]string{
				"paas.io/e2e": "true",
				// Talos enforces the baseline Pod Security standard, which
				// rejects hostNetwork — and a hostNetwork pod is the only way
				// to test the path containerd itself takes to the registry
				// ClusterIP. These namespaces exist for one test each and are
				// deleted with it. Tenant namespaces are a phase-2 concern and
				// are labelled by the Tenant reconciler, not here.
				"pod-security.kubernetes.io/enforce": "privileged",
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := clientset.CoreV1().Namespaces().Delete(ctx, ns.Name, metav1.DeleteOptions{}); err != nil {
			t.Logf("cleanup: delete namespace %s: %v", ns.Name, err)
		}
	})
	return ns.Name
}

// What a failing storage test needs to be diagnosed from CI logs alone.
func dumpNamespace(t *testing.T, ns string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	if pods, err := clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{}); err == nil {
		for _, p := range pods.Items {
			t.Logf("pod/%s node=%q phase=%s", p.Name, p.Spec.NodeName, p.Status.Phase)
			for _, cs := range p.Status.ContainerStatuses {
				t.Logf("  container %s ready=%t restarts=%d state=%+v",
					cs.Name, cs.Ready, cs.RestartCount, cs.State)
			}
		}
	}

	if pvcs, err := clientset.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{}); err == nil {
		for _, c := range pvcs.Items {
			t.Logf("pvc/%s phase=%s volume=%q", c.Name, c.Status.Phase, c.Spec.VolumeName)
		}
	}

	if events, err := clientset.CoreV1().Events(ns).List(ctx, metav1.ListOptions{}); err == nil {
		for _, e := range events.Items {
			if e.Type == corev1.EventTypeNormal {
				continue // warnings are what explain a stuck volume
			}
			t.Logf("event %s/%s: %s: %s", e.InvolvedObject.Kind, e.InvolvedObject.Name, e.Reason, e.Message)
		}
	}
}
