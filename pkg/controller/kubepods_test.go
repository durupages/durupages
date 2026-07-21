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
