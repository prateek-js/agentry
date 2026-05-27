package provisioner

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// ResourceList mirrors the subset of Kubernetes resource keys callers can
// override on a per-sandbox basis.
//
// All fields are quantity strings parsed by resource.ParseQuantity, e.g.
// "500m" (CPU), "1Gi" (memory), "4Gi" (ephemeral-storage), "1" (GPU).
//
// Empty fields fall back to the provisioner's defaults.
type ResourceList struct {
	CPU              string `json:"cpu,omitempty"`
	Memory           string `json:"memory,omitempty"`
	EphemeralStorage string `json:"ephemeral_storage,omitempty"`
	GPU              string `json:"gpu,omitempty"`
}

// Resources captures per-sandbox resource overrides.
type Resources struct {
	Requests *ResourceList `json:"requests,omitempty"`
	Limits   *ResourceList `json:"limits,omitempty"`
}

// gpuResourceName is the Kubernetes extended resource for NVIDIA GPUs.
// (Other GPU vendors use different names — when we need AMD/Intel/etc.,
// extend ResourceList with a vendor field or a generic map.)
const gpuResourceName corev1.ResourceName = "nvidia.com/gpu"

// defaultResources returns the baseline resource requirements applied when
// the caller does not override them. Sized for the typical sandbox workload:
// pip / npm installs, pandas / matplotlib over a few-million-row Trino result,
// a Vite dev server alongside a FastAPI backend, plus occasional buildah
// image builds. Tighter caps land projects in OOM territory fast.
//
// Override per-deployment via env: SANDBOX_DEFAULT_CPU_LIMIT,
// SANDBOX_DEFAULT_MEMORY_LIMIT, SANDBOX_DEFAULT_STORAGE_LIMIT (and the
// matching _REQUEST forms). Override per-sandbox via CreateRequest.Resources.
func defaultResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:              resource.MustParse(envOr("SANDBOX_DEFAULT_CPU_REQUEST", "1")),
			corev1.ResourceMemory:           resource.MustParse(envOr("SANDBOX_DEFAULT_MEMORY_REQUEST", "2Gi")),
			corev1.ResourceEphemeralStorage: resource.MustParse(envOr("SANDBOX_DEFAULT_STORAGE_REQUEST", "8Gi")),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:              resource.MustParse(envOr("SANDBOX_DEFAULT_CPU_LIMIT", "8")),
			corev1.ResourceMemory:           resource.MustParse(envOr("SANDBOX_DEFAULT_MEMORY_LIMIT", "12Gi")),
			corev1.ResourceEphemeralStorage: resource.MustParse(envOr("SANDBOX_DEFAULT_STORAGE_LIMIT", "32Gi")),
		},
	}
}

// buildResourceRequirements merges user-supplied overrides with defaults.
// Returns an error if any quantity string fails to parse — callers should
// surface this as a 400 to the user, not a 500.
func buildResourceRequirements(user *Resources) (corev1.ResourceRequirements, error) {
	out := defaultResources()
	if user == nil {
		return out, nil
	}
	if user.Requests != nil {
		if err := applyResourceList(out.Requests, user.Requests, "requests"); err != nil {
			return corev1.ResourceRequirements{}, err
		}
	}
	if user.Limits != nil {
		if err := applyResourceList(out.Limits, user.Limits, "limits"); err != nil {
			return corev1.ResourceRequirements{}, err
		}
	}
	return out, nil
}

func applyResourceList(dst corev1.ResourceList, src *ResourceList, side string) error {
	if src.CPU != "" {
		q, err := resource.ParseQuantity(src.CPU)
		if err != nil {
			return fmt.Errorf("invalid %s.cpu %q: %w", side, src.CPU, err)
		}
		dst[corev1.ResourceCPU] = q
	}
	if src.Memory != "" {
		q, err := resource.ParseQuantity(src.Memory)
		if err != nil {
			return fmt.Errorf("invalid %s.memory %q: %w", side, src.Memory, err)
		}
		dst[corev1.ResourceMemory] = q
	}
	if src.EphemeralStorage != "" {
		q, err := resource.ParseQuantity(src.EphemeralStorage)
		if err != nil {
			return fmt.Errorf("invalid %s.ephemeral_storage %q: %w", side, src.EphemeralStorage, err)
		}
		dst[corev1.ResourceEphemeralStorage] = q
	}
	if src.GPU != "" {
		q, err := resource.ParseQuantity(src.GPU)
		if err != nil {
			return fmt.Errorf("invalid %s.gpu %q: %w", side, src.GPU, err)
		}
		dst[gpuResourceName] = q
	}
	return nil
}
