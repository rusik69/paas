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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rusik69/paas/pkg/wait"
)

const (
	payloadPath  = "/data/payload"
	checksumPath = "/data/payload.sha256"
	readyMarker  = "/data/.ready"
)

// The phase-0 exit criterion from roadmap.md: a replicated-3 PVC binds, and the
// data survives an unclean power-off of the node holding the primary replica.
func TestStorage_Replicated3SurvivesLossOfPrimaryNode(t *testing.T) {
	nodes := topology(t)
	ns := namespace(t, "e2e-storage")
	t.Cleanup(func() {
		if t.Failed() {
			dumpNamespace(t, ns)
		}
	})

	createPVC(t, ns, "replicated", "replicated-3", "1Gi")

	// The checksum is written by the same pod that wrote the payload, so a
	// reader verifying it proves the bytes survived. A reader that merely found
	// a non-empty file would also pass against a silently re-created volume.
	writer := podSpec("writer", "replicated", []string{"sh", "-c", fmt.Sprintf(
		"head -c 4194304 /dev/urandom > %s && sha256sum %s > %s && sync && touch %s && sleep infinity",
		payloadPath, payloadPath, checksumPath, readyMarker)})
	writer.Spec.Containers[0].ReadinessProbe = execProbe([]string{"test", "-f", readyMarker})
	createPod(t, ns, writer)

	if err := waitPodReady(t, ns, "writer", 10*time.Minute); err != nil {
		t.Fatalf("writer never became ready — the replicated-3 PVC did not bind: %v", err)
	}

	// Keeping the volume mounted is what makes this node genuinely the DRBD
	// primary when it is killed.
	primary := getPod(t, ns, "writer").Spec.NodeName
	if primary == "" {
		t.Fatal("writer pod has no nodeName")
	}
	domain := domainForNode(t, nodes, primary)
	t.Logf("primary replica is mounted on node %s (libvirt domain %s)", primary, domain)

	powerOff(t, domain)

	// Unknown, not False: False would mean the kubelet is running and
	// unhealthy, which is a different failure and would mean the power-off did
	// not take.
	if err := waitNodeReadyIs(t, primary, corev1.ConditionUnknown, 5*time.Minute); err != nil {
		t.Fatalf("node %s never left Ready after power-off: %v", primary, err)
	}

	// The pod object outlives its node, and eviction waits out the unreachable
	// toleration. Force-deleting is what an operator does, and what the App
	// controller will do in phase 4.
	grace := int64(0)
	if err := clientset.CoreV1().Pods(ns).Delete(t.Context(), "writer",
		metav1.DeleteOptions{GracePeriodSeconds: &grace}); err != nil {
		t.Fatalf("force delete writer: %v", err)
	}

	// restartPolicy Never plus `sha256sum -c` makes Succeeded a checksum match
	// and Failed corruption, rather than a generic "pod is unhappy".
	reader := podSpec("reader", "replicated", []string{
		"sh", "-c",
		fmt.Sprintf("cd / && sha256sum -c %s", checksumPath),
	})
	reader.Spec.RestartPolicy = corev1.RestartPolicyNever
	createPod(t, ns, reader)

	phase, err := waitPodTerminated(t, ns, "reader", 10*time.Minute)
	if err != nil {
		t.Fatalf("reader pod did not terminate: %v", err)
	}
	if phase != corev1.PodSucceeded {
		t.Fatalf("reader pod phase = %s, want %s — the data did not survive loss of the primary replica",
			phase, corev1.PodSucceeded)
	}
	if got := getPod(t, ns, "reader").Spec.NodeName; got == primary {
		t.Errorf("reader scheduled onto the killed node %s — the test proved nothing", got)
	}

	// A cluster that survives the failure but never heals has only deferred the
	// outage.
	powerOn(t, domain)
	if err := waitNodeReadyIs(t, primary, corev1.ConditionTrue, 10*time.Minute); err != nil {
		t.Fatalf("node %s did not rejoin after power-on: %v", primary, err)
	}
	if err := waitReplicasUpToDate(t, 10*time.Minute); err != nil {
		t.Fatalf("DRBD did not resync after the node returned: %v", err)
	}
}

// Binding only: failover behaviour is identical to replicated-3 and re-testing
// it would double the suite's runtime for no new information.
func TestStorage_Replicated2Binds(t *testing.T) {
	ns := namespace(t, "e2e-storage-r2")
	t.Cleanup(func() {
		if t.Failed() {
			dumpNamespace(t, ns)
		}
	})

	createPVC(t, ns, "replicated", "replicated-2", "1Gi")
	pod := podSpec("consumer", "replicated", []string{"sh", "-c", "touch /data/ok && sleep infinity"})
	pod.Spec.Containers[0].ReadinessProbe = execProbe([]string{"test", "-f", "/data/ok"})
	createPod(t, ns, pod)

	if err := waitPodReady(t, ns, "consumer", 10*time.Minute); err != nil {
		t.Fatalf("replicated-2 PVC did not become usable: %v", err)
	}
}

func createPVC(t *testing.T, ns, name, class, size string) {
	t.Helper()

	_, err := clientset.CoreV1().PersistentVolumeClaims(ns).Create(t.Context(), &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &class,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)},
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create pvc %s/%s: %v", ns, name, err)
	}
}

func podSpec(name, pvc string, command []string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyAlways,
			Containers: []corev1.Container{{
				Name:         "main",
				Image:        *busyboxImage,
				Command:      command,
				VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/data"}},
			}},
			Volumes: []corev1.Volume{{
				Name: "data",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvc},
				},
			}},
		},
	}
}

func execProbe(command []string) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler:        corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: command}},
		PeriodSeconds:       2,
		FailureThreshold:    120,
		InitialDelaySeconds: 1,
	}
}

func createPod(t *testing.T, ns string, pod *corev1.Pod) {
	t.Helper()

	if _, err := clientset.CoreV1().Pods(ns).Create(t.Context(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pod %s/%s: %v", ns, pod.Name, err)
	}
}

func getPod(t *testing.T, ns, name string) *corev1.Pod {
	t.Helper()

	pod, err := clientset.CoreV1().Pods(ns).Get(t.Context(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod %s/%s: %v", ns, name, err)
	}
	return pod
}

// Matched by InternalIP. Matching on name would work today because DHCP
// supplies the hostname, but that couples the test to dnsmasq behaviour; the
// address is what the topology actually pins.
func domainForNode(t *testing.T, nodes []node, nodeName string) string {
	t.Helper()

	n, err := clientset.CoreV1().Nodes().Get(t.Context(), nodeName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get node %s: %v", nodeName, err)
	}
	for _, addr := range n.Status.Addresses {
		if addr.Type != corev1.NodeInternalIP {
			continue
		}
		for _, topo := range nodes {
			if topo.IP == addr.Address {
				return topo.Domain
			}
		}
	}
	t.Fatalf("node %s has no address matching the e2e topology %+v", nodeName, nodes)
	return ""
}

func waitPodReady(t *testing.T, ns, name string, timeout time.Duration) error {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()

	return wait.For(ctx, 5*time.Second, "pod "+ns+"/"+name+" Ready", func(ctx context.Context) (bool, error) {
		pod, err := clientset.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, nil // transient during a node failure
		}
		for _, c := range pod.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				return true, nil
			}
		}
		return false, nil
	})
}

func waitPodTerminated(t *testing.T, ns, name string, timeout time.Duration) (corev1.PodPhase, error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()

	var phase corev1.PodPhase
	err := wait.For(ctx, 5*time.Second, "pod "+ns+"/"+name+" to terminate", func(ctx context.Context) (bool, error) {
		pod, err := clientset.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		phase = pod.Status.Phase
		return phase == corev1.PodSucceeded || phase == corev1.PodFailed, nil
	})
	return phase, err
}

func waitNodeReadyIs(t *testing.T, name string, want corev1.ConditionStatus, timeout time.Duration) error {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()

	what := fmt.Sprintf("node %s Ready=%s", name, want)
	return wait.For(ctx, 5*time.Second, what, func(ctx context.Context) (bool, error) {
		n, err := clientset.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady {
				return c.Status == want, nil
			}
		}
		return false, nil
	})
}

// LINSTOR is asked directly: a PV that is Bound says nothing about whether the
// returning node's replica has caught up.
func waitReplicasUpToDate(t *testing.T, timeout time.Duration) error {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()

	return wait.Stable(ctx, 10*time.Second, 20*time.Second, "all DRBD replicas UpToDate",
		func(ctx context.Context) (bool, error) {
			out, err := linstor(ctx, "resource", "list-volumes")
			if err != nil {
				return false, nil
			}
			for _, bad := range []string{"Inconsistent", "Outdated", "SyncTarget", "Unknown"} {
				if strings.Contains(out, bad) {
					return false, nil
				}
			}
			return strings.Contains(out, "UpToDate"), nil
		})
}

func linstor(ctx context.Context, args ...string) (string, error) {
	pod, err := exec.CommandContext(ctx, "kubectl", "get", "pods", "-n", "piraeus-datastore",
		"-l", "app.kubernetes.io/component=linstor-controller",
		"-o", "jsonpath={.items[0].metadata.name}").Output()
	if err != nil {
		return "", fmt.Errorf("find linstor controller: %w", err)
	}
	if len(pod) == 0 {
		return "", fmt.Errorf("no linstor-controller pod")
	}

	full := append([]string{"exec", "-n", "piraeus-datastore", string(pod), "--", "linstor", "--no-color"}, args...)
	out, err := exec.CommandContext(ctx, "kubectl", full...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("linstor %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}
