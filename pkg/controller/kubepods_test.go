// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func newKubePodsForTest(t *testing.T) (*kubePods, *fake.Clientset) {
	t.Helper()
	cs := fake.NewSimpleClientset()
	kp, err := NewKubePods(KubePodsOptions{
		Client:             cs,
		Namespace:          "durupages-workers",
		Image:              "durupages/worker:latest",
		ServiceAccountName: "durupages-worker-noperm",
		Generation:         "gen-abc123",
		DefaultCPULimit:    "1",
		DefaultMemLimit:    "256Mi",
	})
	if err != nil {
		t.Fatalf("NewKubePods: %v", err)
	}
	return kp, cs
}

// newKubePodsWithOverrides is newKubePodsForTest plus cluster-wide
// WorkerPodOverrides.
func newKubePodsWithOverrides(t *testing.T, ov WorkerPodOverrides) (*kubePods, *fake.Clientset) {
	t.Helper()
	cs := fake.NewSimpleClientset()
	kp, err := NewKubePods(KubePodsOptions{
		Client:             cs,
		Namespace:          "durupages-workers",
		Image:              "durupages/worker:latest",
		Generation:         "gen-abc123",
		CommonPodOverrides: ov,
	})
	if err != nil {
		t.Fatalf("NewKubePods: %v", err)
	}
	return kp, cs
}

// newKubePodsWithCommonAnnotations is newKubePodsWithOverrides for the
// annotations-only case, which most of the older tests below exercise.
func newKubePodsWithCommonAnnotations(t *testing.T, common map[string]string) (*kubePods, *fake.Clientset) {
	t.Helper()
	return newKubePodsWithOverrides(t, WorkerPodOverrides{Annotations: common})
}

// createdPod creates spec and returns the resulting pod.
func createdPod(t *testing.T, kp *kubePods, cs *fake.Clientset, spec PodSpec) *corev1.Pod {
	t.Helper()
	if err := kp.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	pod, err := cs.CoreV1().Pods("durupages-workers").Get(context.Background(), spec.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return pod
}

// TestKubePodsCommonAnnotationsOnly checks operator annotations reach a pod
// whose tenant declared none.
func TestKubePodsCommonAnnotationsOnly(t *testing.T) {
	kp, cs := newKubePodsWithCommonAnnotations(t, map[string]string{
		"prometheus.io/scrape": "true",
		"prometheus.io/port":   "9090",
	})
	pod := createdPod(t, kp, cs, PodSpec{Name: "p", TenantID: "acme"})
	if pod.Annotations["prometheus.io/scrape"] != "true" || pod.Annotations["prometheus.io/port"] != "9090" {
		t.Fatalf("common annotations missing: %v", pod.Annotations)
	}
}

// TestKubePodsTenantAnnotationsWithoutCommon checks the feature stays out of
// the way when no common set is configured.
func TestKubePodsTenantAnnotationsWithoutCommon(t *testing.T) {
	kp, cs := newKubePodsWithCommonAnnotations(t, nil)
	pod := createdPod(t, kp, cs, PodSpec{
		Name: "p", TenantID: "acme",
		Annotations: map[string]string{"example.com/note": "hi"},
	})
	if len(pod.Annotations) != 1 || pod.Annotations["example.com/note"] != "hi" {
		t.Fatalf("tenant annotations wrong: %v", pod.Annotations)
	}
}

// TestKubePodsCommonAnnotationsWinOverTenant pins the precedence: the operator's
// cluster-wide policy is not something a tenant can opt out of by naming the
// same key, while keys the operator has not claimed still belong to the tenant.
func TestKubePodsCommonAnnotationsWinOverTenant(t *testing.T) {
	kp, cs := newKubePodsWithCommonAnnotations(t, map[string]string{
		"prometheus.io/scrape":    "true",
		"sidecar.istio.io/inject": "false",
	})
	pod := createdPod(t, kp, cs, PodSpec{
		Name: "p", TenantID: "acme",
		Annotations: map[string]string{
			"prometheus.io/scrape": "false", // tenant tries to opt out
			"example.com/note":     "hi",    // tenant's own key, untouched
		},
	})
	if pod.Annotations["prometheus.io/scrape"] != "true" {
		t.Fatalf("tenant overrode a common annotation: %v", pod.Annotations)
	}
	if pod.Annotations["sidecar.istio.io/inject"] != "false" {
		t.Fatalf("common annotation missing: %v", pod.Annotations)
	}
	if pod.Annotations["example.com/note"] != "hi" {
		t.Fatalf("tenant annotation lost: %v", pod.Annotations)
	}
}

// TestKubePodsCommonAnnotationsLeaveSystemLabelsAlone checks the labels that
// reconcile relies on are unaffected by the annotation merge.
func TestKubePodsCommonAnnotationsLeaveSystemLabelsAlone(t *testing.T) {
	kp, cs := newKubePodsWithCommonAnnotations(t, map[string]string{"prometheus.io/scrape": "true"})
	pod := createdPod(t, kp, cs, PodSpec{
		Name: "p", TenantID: "acme",
		Labels: map[string]string{"team": "web"},
	})
	if pod.Labels[labelAppName] != appNameWorker || pod.Labels[labelTenantID] != "acme" ||
		pod.Labels[labelGeneration] != "gen-abc123" {
		t.Fatalf("system labels disturbed: %v", pod.Labels)
	}
	if pod.Labels["team"] != "web" {
		t.Fatalf("tenant label lost: %v", pod.Labels)
	}
	if _, ok := pod.Labels["prometheus.io/scrape"]; ok {
		t.Fatalf("annotation leaked into labels: %v", pod.Labels)
	}
}

// TestKubePodsCommonLabelsWinOverTenant mirrors the annotation precedence test
// for labels: the operator's cluster-wide set wins on collision, and the
// system labels reconcile relies on remain untouched by either.
func TestKubePodsCommonLabelsWinOverTenant(t *testing.T) {
	kp, cs := newKubePodsWithOverrides(t, WorkerPodOverrides{
		Labels: map[string]string{"team": "platform", "cost-center": "infra"},
	})
	pod := createdPod(t, kp, cs, PodSpec{
		Name: "p", TenantID: "acme",
		Labels: map[string]string{"team": "acme-web", "app": "blog"},
	})
	if pod.Labels["team"] != "platform" {
		t.Fatalf("tenant overrode a common label: %v", pod.Labels)
	}
	if pod.Labels["cost-center"] != "infra" || pod.Labels["app"] != "blog" {
		t.Fatalf("labels merged wrong: %v", pod.Labels)
	}
	if pod.Labels[labelAppName] != appNameWorker || pod.Labels[labelTenantID] != "acme" {
		t.Fatalf("system labels disturbed: %v", pod.Labels)
	}
}

// TestKubePodsCommonPodOverridesApplyScheduling checks that the
// scheduling/placement fields -- which have no tenant-level equivalent to
// merge with -- land on the pod spec verbatim.
func TestKubePodsCommonPodOverridesApplyScheduling(t *testing.T) {
	rc := "gvisor"
	ov := WorkerPodOverrides{
		NodeSelector: map[string]string{"workload": "durupages-worker"},
		Tolerations: []corev1.Toleration{{
			Key: "durupages.io/dedicated", Operator: corev1.TolerationOpEqual,
			Value: "true", Effect: corev1.TaintEffectNoSchedule,
		}},
		Affinity: &corev1.Affinity{
			NodeAffinity: &corev1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{{
						MatchExpressions: []corev1.NodeSelectorRequirement{{
							Key: "kubernetes.io/arch", Operator: corev1.NodeSelectorOpIn, Values: []string{"amd64"},
						}},
					}},
				},
			},
		},
		DNSPolicy:         corev1.DNSNone,
		DNSConfig:         &corev1.PodDNSConfig{Nameservers: []string{"10.0.0.10"}},
		PriorityClassName: "high-priority",
		RuntimeClassName:  &rc,
	}
	kp, cs := newKubePodsWithOverrides(t, ov)
	pod := createdPod(t, kp, cs, PodSpec{Name: "p", TenantID: "acme"})

	if pod.Spec.NodeSelector["workload"] != "durupages-worker" {
		t.Fatalf("nodeSelector missing: %v", pod.Spec.NodeSelector)
	}
	if len(pod.Spec.Tolerations) != 1 || pod.Spec.Tolerations[0].Key != "durupages.io/dedicated" {
		t.Fatalf("tolerations missing: %v", pod.Spec.Tolerations)
	}
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.NodeAffinity == nil {
		t.Fatalf("affinity missing: %v", pod.Spec.Affinity)
	}
	if pod.Spec.DNSPolicy != corev1.DNSNone || pod.Spec.DNSConfig == nil ||
		len(pod.Spec.DNSConfig.Nameservers) != 1 || pod.Spec.DNSConfig.Nameservers[0] != "10.0.0.10" {
		t.Fatalf("dns config wrong: policy=%v config=%v", pod.Spec.DNSPolicy, pod.Spec.DNSConfig)
	}
	if pod.Spec.PriorityClassName != "high-priority" {
		t.Fatalf("priorityClassName missing: %v", pod.Spec.PriorityClassName)
	}
	if pod.Spec.RuntimeClassName == nil || *pod.Spec.RuntimeClassName != "gvisor" {
		t.Fatalf("runtimeClassName missing: %v", pod.Spec.RuntimeClassName)
	}
}

// TestKubePodsCommonPodOverridesLeaveCoreSpecAlone guards the allowlist: no
// combination of overrides may reach the container, volumes, security
// context, service account or restart policy the controller itself sets --
// those are what keeps every worker pod fungible and isolated.
func TestKubePodsCommonPodOverridesLeaveCoreSpecAlone(t *testing.T) {
	kp, cs := newKubePodsWithOverrides(t, WorkerPodOverrides{
		NodeSelector: map[string]string{"workload": "durupages-worker"},
	})
	pod := createdPod(t, kp, cs, PodSpec{Name: "p", TenantID: "acme"})

	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatalf("RestartPolicy = %v", pod.Spec.RestartPolicy)
	}
	if len(pod.Spec.Containers) != 1 || pod.Spec.Containers[0].Name != workerContainerName {
		t.Fatalf("containers disturbed: %v", pod.Spec.Containers)
	}
	if pod.Spec.SecurityContext == nil || !*pod.Spec.SecurityContext.RunAsNonRoot {
		t.Fatalf("pod security context disturbed: %v", pod.Spec.SecurityContext)
	}
}

// TestNewKubePodsRejectsBadCommonAnnotations checks the validation also guards
// callers that build the map themselves rather than reading the file.
func TestNewKubePodsRejectsBadCommonAnnotations(t *testing.T) {
	for _, common := range []map[string]string{
		{"bad key!": "x"},
		{labelTenantID: "evil"},
		{"app.kubernetes.io/name": "spoof"},
	} {
		_, err := NewKubePods(KubePodsOptions{
			Client:             fake.NewSimpleClientset(),
			Namespace:          "durupages-workers",
			Image:              "durupages/worker:latest",
			CommonPodOverrides: WorkerPodOverrides{Annotations: common},
		})
		if err == nil {
			t.Fatalf("NewKubePods accepted %v", common)
		}
	}
}

func TestKubePodsCreateBuildsHardenedPod(t *testing.T) {
	kp, cs := newKubePodsForTest(t)
	ctx := context.Background()

	spec := PodSpec{
		Name:        "dpw-acme-abc123",
		TenantID:    "acme",
		Labels:      map[string]string{"team": "web", "cost-center": "42"},
		Annotations: map[string]string{"example.com/note": "hi"},
		Env:         map[string]string{"DURUPAGES_TENANT_ID": "acme", "DURUPAGES_POD_NAME": "dpw-acme-abc123"},
		CPULimit:    "2",
		MemLimit:    "512Mi",
	}
	if err := kp.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	pod, err := cs.CoreV1().Pods("durupages-workers").Get(ctx, "dpw-acme-abc123", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	// System labels present (and win) plus tenant labels.
	if pod.Labels[labelAppName] != appNameWorker ||
		pod.Labels[labelTenantID] != "acme" ||
		pod.Labels[labelGeneration] != "gen-abc123" {
		t.Fatalf("system labels wrong: %v", pod.Labels)
	}
	if pod.Labels["team"] != "web" || pod.Labels["cost-center"] != "42" {
		t.Fatalf("tenant labels missing: %v", pod.Labels)
	}
	if pod.Annotations["example.com/note"] != "hi" {
		t.Fatalf("annotations missing: %v", pod.Annotations)
	}

	// Pod-level security / SA settings.
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Fatal("automountServiceAccountToken must be false")
	}
	if pod.Spec.ServiceAccountName != "durupages-worker-noperm" {
		t.Fatalf("service account = %q", pod.Spec.ServiceAccountName)
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatalf("restart policy = %q", pod.Spec.RestartPolicy)
	}
	if psc := pod.Spec.SecurityContext; psc == nil || psc.RunAsNonRoot == nil || !*psc.RunAsNonRoot ||
		psc.SeccompProfile == nil || psc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("pod security context wrong: %+v", pod.Spec.SecurityContext)
	}

	// The writable /bundles emptyDir volume.
	if len(pod.Spec.Volumes) != 1 || pod.Spec.Volumes[0].Name != bundlesVolumeName || pod.Spec.Volumes[0].EmptyDir == nil {
		t.Fatalf("bundles volume wrong: %+v", pod.Spec.Volumes)
	}

	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("want 1 container, got %d", len(pod.Spec.Containers))
	}
	ct := pod.Spec.Containers[0]
	if ct.Image != "durupages/worker:latest" {
		t.Fatalf("image = %q", ct.Image)
	}
	// Container security context hardening.
	sc := ct.SecurityContext
	if sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation ||
		sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem ||
		sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot ||
		sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("container security context wrong: %+v", sc)
	}
	// Volume mount at /bundles.
	if len(ct.VolumeMounts) != 1 || ct.VolumeMounts[0].MountPath != bundlesMountPath {
		t.Fatalf("bundles mount wrong: %+v", ct.VolumeMounts)
	}
	// Env passed through, deterministically ordered.
	envByName := map[string]string{}
	for _, ev := range ct.Env {
		envByName[ev.Name] = ev.Value
	}
	if envByName["DURUPAGES_TENANT_ID"] != "acme" || envByName["DURUPAGES_POD_NAME"] != "dpw-acme-abc123" {
		t.Fatalf("env wrong: %v", ct.Env)
	}
	// Resource limits from the spec override defaults.
	if q := ct.Resources.Limits[corev1.ResourceCPU]; q.String() != "2" {
		t.Fatalf("cpu limit = %s", q.String())
	}
	if q := ct.Resources.Limits[corev1.ResourceMemory]; q.String() != "512Mi" {
		t.Fatalf("mem limit = %s", q.String())
	}
}

func TestKubePodsCreateUsesDefaultResources(t *testing.T) {
	kp, cs := newKubePodsForTest(t)
	ctx := context.Background()
	if err := kp.Create(ctx, PodSpec{Name: "p", TenantID: "acme"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	pod, _ := cs.CoreV1().Pods("durupages-workers").Get(ctx, "p", metav1.GetOptions{})
	ct := pod.Spec.Containers[0]
	if q := ct.Resources.Limits[corev1.ResourceCPU]; q.String() != "1" {
		t.Fatalf("default cpu = %s", q.String())
	}
	if q := ct.Resources.Limits[corev1.ResourceMemory]; q.String() != "256Mi" {
		t.Fatalf("default mem = %s", q.String())
	}
}

func TestKubePodsRejectsSystemLabelConflict(t *testing.T) {
	kp, _ := newKubePodsForTest(t)
	ctx := context.Background()

	err := kp.Create(ctx, PodSpec{
		Name:     "p",
		TenantID: "acme",
		Labels:   map[string]string{labelTenantID: "evil"},
	})
	if err == nil {
		t.Fatal("expected rejection of conflicting system label")
	}

	err = kp.Create(ctx, PodSpec{
		Name:     "p2",
		TenantID: "acme",
		Labels:   map[string]string{"app.kubernetes.io/name": "spoof"},
	})
	if err == nil {
		t.Fatal("expected rejection of app.kubernetes.io/* label")
	}
}

func TestKubePodsListFiltersAndMapsTenant(t *testing.T) {
	kp, cs := newKubePodsForTest(t)
	ctx := context.Background()

	// One worker pod, one unrelated pod.
	if err := kp.Create(ctx, PodSpec{Name: "dpw-acme-1", TenantID: "acme"}); err != nil {
		t.Fatalf("create worker: %v", err)
	}
	_, err := cs.CoreV1().Pods("durupages-workers").Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "unrelated",
			Namespace: "durupages-workers",
			Labels:    map[string]string{"app.kubernetes.io/name": "something-else"},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create unrelated: %v", err)
	}

	pods, err := kp.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(pods) != 1 {
		t.Fatalf("want 1 worker pod, got %d: %+v", len(pods), pods)
	}
	if pods[0].Name != "dpw-acme-1" || pods[0].TenantID != "acme" {
		t.Fatalf("bad existing pod: %+v", pods[0])
	}
}

func TestKubePodsDeleteMissingIsNoError(t *testing.T) {
	kp, _ := newKubePodsForTest(t)
	if err := kp.Delete(context.Background(), "nope"); err != nil {
		t.Fatalf("delete missing: %v", err)
	}
}
