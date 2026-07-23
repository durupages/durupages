// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package controller

import "context"

// PodManager abstracts worker pod lifecycle so the dispatcher can be unit
// tested without a real Kubernetes cluster. The default implementation is
// kubePods (see kubepods.go); tests supply a recording fake.
type PodManager interface {
	// Create schedules a new bare worker pod from spec. It returns once the
	// create request has been accepted (the pod is not necessarily Ready).
	Create(ctx context.Context, spec PodSpec) error
	// Delete removes the pod with the given name. Deleting a pod that no longer
	// exists is not an error.
	Delete(ctx context.Context, name string) error
	// List returns every worker pod the manager knows about (filtered to the
	// durupages worker app label by the implementation).
	List(ctx context.Context) ([]ExistingPod, error)
}

// PodSpec is the controller-supplied description of a worker pod. Only
// tenant-level, non-system fields live here; infrastructure concerns (image,
// namespace, service account, security context) belong to the PodManager
// implementation and are applied on top. System labels always win over the
// tenant Labels/Annotations supplied here.
type PodSpec struct {
	// Name is the unique pod name (e.g. "dpw-acme-ab12cd").
	Name string
	// TenantID is the owning tenant; it becomes the durupages.io/tenant-id
	// system label.
	TenantID string
	// Labels / Annotations are the tenant's custom metadata (TenantConfig).
	Labels      map[string]string
	Annotations map[string]string
	// Env is injected into the worker container verbatim.
	Env map[string]string
	// CPULimit / MemLimit are k8s quantity strings (e.g. "1", "512Mi"); empty
	// means "use the PodManager default".
	CPULimit string
	MemLimit string
}

// ExistingPod is the reconcile view of a live worker pod.
type ExistingPod struct {
	// Name is the pod name.
	Name string
	// TenantID is read back from the durupages.io/tenant-id label.
	TenantID string
	// Labels is the pod's full label set.
	Labels map[string]string
	// Failed reports a pod whose container has already terminated with a
	// non-zero exit and, since worker pods run with RestartPolicyNever, never
	// will run again. It is a terminal, unambiguous signal: unlike a missing
	// heartbeat (which just as easily means "hasn't gotten there yet" as "never
	// will"), a failed pod cannot recover into a Ready one. See reconcile.go and
	// scaleDownOnce for how it is used to exclude such pods immediately instead
	// of waiting out an adoption or heartbeat window.
	Failed bool
}
