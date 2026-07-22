// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// System label keys and values (ARCHITECTURE 6.1). These always win over the
// tenant's custom PodLabels and cannot be overridden.
const (
	labelAppName    = "app.kubernetes.io/name"
	labelTenantID   = "durupages.io/tenant-id"
	labelGeneration = "durupages.io/controller-generation"
	appNameWorker   = "durupages-worker"

	workerContainerName = "durupages-worker"
	bundlesVolumeName   = "bundles"
	bundlesMountPath    = "/bundles"
)

// systemLabelPrefixes are reserved for the platform; tenant metadata may not use
// them.
var systemLabelPrefixes = []string{"durupages.io/", "app.kubernetes.io/"}

// KubePodsOptions configures the Kubernetes-backed PodManager.
type KubePodsOptions struct {
	// Client is the Kubernetes clientset (use kubernetes/fake in tests).
	Client kubernetes.Interface
	// Namespace is where worker pods are created (required).
	Namespace string
	// Image is the worker container image (shim + durupages-workerd) (required).
	Image string
	// ServiceAccountName is the unprivileged worker service account.
	ServiceAccountName string
	// Generation is the controller generation stamped on pods for reconcile.
	Generation string
	// DefaultCPULimit / DefaultMemLimit apply when a PodSpec omits its own.
	DefaultCPULimit string
	DefaultMemLimit string
	// CommonAnnotations are cluster-wide annotations stamped on every worker
	// pod, whatever the tenant (see LoadWorkerAnnotationsFile). Empty disables
	// the feature. Validated by NewKubePods, since bad keys here would fail
	// every pod creation instead of one.
	CommonAnnotations map[string]string
}

// kubePods is the Kubernetes PodManager: it creates bare pods (no
// Deployment/ReplicaSet) so the controller can drive per-pod lifecycle.
type kubePods struct {
	opts KubePodsOptions
}

var _ PodManager = (*kubePods)(nil)

// NewKubePods validates opts and returns a Kubernetes PodManager.
func NewKubePods(opts KubePodsOptions) (*kubePods, error) {
	if opts.Client == nil {
		return nil, errors.New("controller: KubePods Client is required")
	}
	if opts.Namespace == "" {
		return nil, errors.New("controller: KubePods Namespace is required")
	}
	if opts.Image == "" {
		return nil, errors.New("controller: KubePods Image is required")
	}
	if err := validateWorkerAnnotations(opts.CommonAnnotations); err != nil {
		return nil, err
	}
	return &kubePods{opts: opts}, nil
}

// Create builds and creates a single bare worker pod. Tenant labels/annotations
// are validated (system prefixes rejected) then merged so the system labels
// always win.
//
// Annotation precedence is tenant first, cluster-wide second: where the two
// name the same key, the operator's value stands. The cluster-wide set is a
// platform policy — scrape targets, mesh injection, chargeback — declared once
// for every worker pod in the cluster, and a policy a tenant can opt out of by
// naming the same key in its own PodAnnotations is not a policy. Tenants keep
// every key the operator has not claimed.
func (k *kubePods) Create(ctx context.Context, spec PodSpec) error {
	if err := validateNoSystemKeys(spec.Labels); err != nil {
		return err
	}
	if err := validateNoSystemKeys(spec.Annotations); err != nil {
		return err
	}

	labels := mergeMap(spec.Labels, map[string]string{
		labelAppName:    appNameWorker,
		labelTenantID:   spec.TenantID,
		labelGeneration: k.opts.Generation,
	})
	annotations := mergeMap(spec.Annotations, k.opts.CommonAnnotations)

	resources, err := k.resourceRequirements(spec)
	if err != nil {
		return err
	}

	falseVal := false
	trueVal := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        spec.Name,
			Namespace:   k.opts.Namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: &falseVal,
			ServiceAccountName:           k.opts.ServiceAccountName,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   &trueVal,
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Volumes: []corev1.Volume{{
				Name:         bundlesVolumeName,
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			}},
			Containers: []corev1.Container{{
				Name:      workerContainerName,
				Image:     k.opts.Image,
				Env:       envVars(spec.Env),
				Resources: resources,
				SecurityContext: &corev1.SecurityContext{
					RunAsNonRoot:             &trueVal,
					AllowPrivilegeEscalation: &falseVal,
					ReadOnlyRootFilesystem:   &trueVal,
					SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
				VolumeMounts: []corev1.VolumeMount{{
					Name:      bundlesVolumeName,
					MountPath: bundlesMountPath,
				}},
			}},
		},
	}

	_, err = k.opts.Client.CoreV1().Pods(k.opts.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	return err
}

// Delete removes the named pod. A missing pod is not an error.
func (k *kubePods) Delete(ctx context.Context, name string) error {
	err := k.opts.Client.CoreV1().Pods(k.opts.Namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// List returns worker pods filtered by the app label, mapping the tenant-id
// label into ExistingPod.
func (k *kubePods) List(ctx context.Context) ([]ExistingPod, error) {
	list, err := k.opts.Client.CoreV1().Pods(k.opts.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelAppName + "=" + appNameWorker,
	})
	if err != nil {
		return nil, err
	}
	out := make([]ExistingPod, 0, len(list.Items))
	for i := range list.Items {
		p := &list.Items[i]
		out = append(out, ExistingPod{
			Name:     p.Name,
			TenantID: p.Labels[labelTenantID],
			Labels:   p.Labels,
		})
	}
	return out, nil
}

// resourceRequirements builds container resource limits from the spec, falling
// back to the manager defaults.
func (k *kubePods) resourceRequirements(spec PodSpec) (corev1.ResourceRequirements, error) {
	cpu := spec.CPULimit
	if cpu == "" {
		cpu = k.opts.DefaultCPULimit
	}
	mem := spec.MemLimit
	if mem == "" {
		mem = k.opts.DefaultMemLimit
	}
	limits := corev1.ResourceList{}
	if cpu != "" {
		q, err := resource.ParseQuantity(cpu)
		if err != nil {
			return corev1.ResourceRequirements{}, fmt.Errorf("controller: invalid CPU limit %q: %w", cpu, err)
		}
		limits[corev1.ResourceCPU] = q
	}
	if mem != "" {
		q, err := resource.ParseQuantity(mem)
		if err != nil {
			return corev1.ResourceRequirements{}, fmt.Errorf("controller: invalid memory limit %q: %w", mem, err)
		}
		limits[corev1.ResourceMemory] = q
	}
	if len(limits) == 0 {
		return corev1.ResourceRequirements{}, nil
	}
	return corev1.ResourceRequirements{Limits: limits}, nil
}

// validateNoSystemKeys rejects tenant metadata that collides with reserved
// system prefixes.
func validateNoSystemKeys(m map[string]string) error {
	for k := range m {
		for _, prefix := range systemLabelPrefixes {
			if strings.HasPrefix(k, prefix) {
				return fmt.Errorf("controller: metadata key %q uses reserved system prefix %q", k, prefix)
			}
		}
	}
	return nil
}

// mergeMap copies base then overlays win (win takes precedence). Returns nil
// only when both are empty.
func mergeMap(base, win map[string]string) map[string]string {
	if len(base) == 0 && len(win) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(win))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range win {
		out[k] = v
	}
	return out
}

// envVars converts an env map into a deterministically ordered EnvVar slice.
func envVars(env map[string]string) []corev1.EnvVar {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]corev1.EnvVar, 0, len(keys))
	for _, k := range keys {
		out = append(out, corev1.EnvVar{Name: k, Value: env[k]})
	}
	return out
}
