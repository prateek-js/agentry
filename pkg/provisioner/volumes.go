package provisioner

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Volume describes a single storage source mounted into the sandbox
// container. Exactly one source (PVC/HostPath/ConfigMap/Secret/EmptyDir)
// must be set.
type Volume struct {
	Name      string `json:"name"`
	MountPath string `json:"mount_path"`
	SubPath   string `json:"sub_path,omitempty"`
	ReadOnly  bool   `json:"read_only,omitempty"`

	PVC       *PVCSource       `json:"pvc,omitempty"`
	HostPath  *HostPathSource  `json:"host_path,omitempty"`
	ConfigMap *ConfigMapSource `json:"config_map,omitempty"`
	Secret    *SecretSource    `json:"secret,omitempty"`
	EmptyDir  *EmptyDirSource  `json:"empty_dir,omitempty"`
}

type PVCSource struct {
	ClaimName string `json:"claim_name"`
}

type HostPathSource struct {
	Path string `json:"path"`
	// Type maps to corev1.HostPathType. Valid values include "",
	// "Directory", "File", "DirectoryOrCreate", "FileOrCreate".
	Type string `json:"type,omitempty"`
}

type ConfigMapSource struct {
	Name        string `json:"name"`
	DefaultMode *int32 `json:"default_mode,omitempty"`
	Optional    *bool  `json:"optional,omitempty"`
}

type SecretSource struct {
	Name        string `json:"name"`
	DefaultMode *int32 `json:"default_mode,omitempty"`
	Optional    *bool  `json:"optional,omitempty"`
}

type EmptyDirSource struct {
	// Medium is "" (default node-disk) or "Memory" (tmpfs).
	Medium string `json:"medium,omitempty"`
	// SizeLimit is a resource quantity string (e.g. "1Gi"); empty = unlimited.
	SizeLimit string `json:"size_limit,omitempty"`
}

// dns1123Label is the regex Kubernetes uses for volume names. Stricter
// than path-segment validation — keeps us in sync with API-server rules.
var dns1123Label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// reservedMountPaths lists paths that must never be remounted from a Volume,
// because they back the runtime itself or could shadow host-OS surfaces in
// surprising ways.
var reservedMountPaths = map[string]struct{}{
	"/":     {},
	"/proc": {},
	"/sys":  {},
	"/dev":  {},
}

// mergeDefaultVolumes appends each default volume to req that doesn't
// already collide with a caller-supplied volume (by name or mount path).
// Caller-supplied volumes always win — the defaults are "what you get
// if you don't ask for anything else."
//
// The function never mutates either input slice — callers can keep
// using the originals freely.
func mergeDefaultVolumes(req, defaults []Volume) []Volume {
	if len(defaults) == 0 {
		return req
	}
	names := make(map[string]struct{}, len(req))
	mounts := make(map[string]struct{}, len(req))
	for i := range req {
		names[req[i].Name] = struct{}{}
		mounts[req[i].MountPath] = struct{}{}
	}
	out := make([]Volume, 0, len(req)+len(defaults))
	out = append(out, req...)
	for i := range defaults {
		d := defaults[i]
		if _, dup := names[d.Name]; dup {
			continue
		}
		if _, dup := mounts[d.MountPath]; dup {
			continue
		}
		out = append(out, d)
	}
	return out
}

// validateVolumes runs every per-volume validation and cross-volume invariant
// (unique names, unique mount paths). It returns the first error it finds —
// callers should surface this as a 400.
func validateVolumes(vols []Volume) error {
	names := make(map[string]struct{}, len(vols))
	mounts := make(map[string]struct{}, len(vols))
	for i := range vols {
		v := &vols[i]
		if err := validateVolume(v); err != nil {
			return fmt.Errorf("volumes[%d]: %w", i, err)
		}
		if _, dup := names[v.Name]; dup {
			return fmt.Errorf("volumes[%d]: duplicate name %q", i, v.Name)
		}
		names[v.Name] = struct{}{}
		if _, dup := mounts[v.MountPath]; dup {
			return fmt.Errorf("volumes[%d]: duplicate mount_path %q", i, v.MountPath)
		}
		mounts[v.MountPath] = struct{}{}
	}
	return nil
}

func validateVolume(v *Volume) error {
	if v.Name == "" {
		return fmt.Errorf("name is required")
	}
	if !dns1123Label.MatchString(v.Name) {
		return fmt.Errorf("name %q is not a DNS-1123 label", v.Name)
	}
	if len(v.Name) > 63 {
		return fmt.Errorf("name %q exceeds 63 chars", v.Name)
	}
	if v.MountPath == "" {
		return fmt.Errorf("mount_path is required")
	}
	if !path.IsAbs(v.MountPath) {
		return fmt.Errorf("mount_path %q must be absolute", v.MountPath)
	}
	if cleaned := path.Clean(v.MountPath); cleaned != v.MountPath {
		return fmt.Errorf("mount_path %q is not in canonical form (expected %q)",
			v.MountPath, cleaned)
	}
	if _, reserved := reservedMountPaths[v.MountPath]; reserved {
		return fmt.Errorf("mount_path %q is reserved", v.MountPath)
	}
	if v.SubPath != "" {
		if path.IsAbs(v.SubPath) {
			return fmt.Errorf("sub_path %q must be relative", v.SubPath)
		}
		if strings.HasPrefix(v.SubPath, "../") || v.SubPath == ".." ||
			strings.Contains(v.SubPath, "/../") || strings.HasSuffix(v.SubPath, "/..") {
			return fmt.Errorf("sub_path %q must not contain '..'", v.SubPath)
		}
	}
	return validateVolumeSource(v)
}

func validateVolumeSource(v *Volume) error {
	count := 0
	if v.PVC != nil {
		count++
		if v.PVC.ClaimName == "" {
			return fmt.Errorf("pvc.claim_name is required")
		}
	}
	if v.HostPath != nil {
		count++
		if v.HostPath.Path == "" {
			return fmt.Errorf("host_path.path is required")
		}
		if !path.IsAbs(v.HostPath.Path) {
			return fmt.Errorf("host_path.path %q must be absolute", v.HostPath.Path)
		}
	}
	if v.ConfigMap != nil {
		count++
		if v.ConfigMap.Name == "" {
			return fmt.Errorf("config_map.name is required")
		}
	}
	if v.Secret != nil {
		count++
		if v.Secret.Name == "" {
			return fmt.Errorf("secret.name is required")
		}
	}
	if v.EmptyDir != nil {
		count++
		if v.EmptyDir.Medium != "" && v.EmptyDir.Medium != "Memory" {
			return fmt.Errorf("empty_dir.medium %q must be \"\" or \"Memory\"",
				v.EmptyDir.Medium)
		}
		if v.EmptyDir.SizeLimit != "" {
			if _, err := resource.ParseQuantity(v.EmptyDir.SizeLimit); err != nil {
				return fmt.Errorf("empty_dir.size_limit %q: %w",
					v.EmptyDir.SizeLimit, err)
			}
		}
	}
	if count == 0 {
		return fmt.Errorf("a volume source (pvc/host_path/config_map/secret/empty_dir) is required")
	}
	if count > 1 {
		return fmt.Errorf("exactly one volume source must be set (got %d)", count)
	}
	return nil
}

// buildVolumes maps the API-facing Volume slice onto corev1.Volume +
// corev1.VolumeMount slices. Callers must have already run validateVolumes.
func buildVolumes(vols []Volume) ([]corev1.Volume, []corev1.VolumeMount, error) {
	if len(vols) == 0 {
		return nil, nil, nil
	}
	out := make([]corev1.Volume, 0, len(vols))
	mounts := make([]corev1.VolumeMount, 0, len(vols))
	for i := range vols {
		v := &vols[i]
		k8sVol, err := buildVolumeSource(v)
		if err != nil {
			return nil, nil, fmt.Errorf("volumes[%d]: %w", i, err)
		}
		out = append(out, k8sVol)
		mounts = append(mounts, corev1.VolumeMount{
			Name:      v.Name,
			MountPath: v.MountPath,
			SubPath:   v.SubPath,
			ReadOnly:  v.ReadOnly,
		})
	}
	return out, mounts, nil
}

func buildVolumeSource(v *Volume) (corev1.Volume, error) {
	out := corev1.Volume{Name: v.Name}
	switch {
	case v.PVC != nil:
		out.PersistentVolumeClaim = &corev1.PersistentVolumeClaimVolumeSource{
			ClaimName: v.PVC.ClaimName,
			ReadOnly:  v.ReadOnly,
		}
	case v.HostPath != nil:
		hpt := corev1.HostPathType(v.HostPath.Type)
		out.HostPath = &corev1.HostPathVolumeSource{
			Path: v.HostPath.Path,
			Type: &hpt,
		}
	case v.ConfigMap != nil:
		out.ConfigMap = &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: v.ConfigMap.Name},
			DefaultMode:          v.ConfigMap.DefaultMode,
			Optional:             v.ConfigMap.Optional,
		}
	case v.Secret != nil:
		out.Secret = &corev1.SecretVolumeSource{
			SecretName:  v.Secret.Name,
			DefaultMode: v.Secret.DefaultMode,
			Optional:    v.Secret.Optional,
		}
	case v.EmptyDir != nil:
		ed := &corev1.EmptyDirVolumeSource{
			Medium: corev1.StorageMedium(v.EmptyDir.Medium),
		}
		if v.EmptyDir.SizeLimit != "" {
			q, err := resource.ParseQuantity(v.EmptyDir.SizeLimit)
			if err != nil {
				return corev1.Volume{}, fmt.Errorf("empty_dir.size_limit: %w", err)
			}
			ed.SizeLimit = &q
		}
		out.EmptyDir = ed
	default:
		return corev1.Volume{}, fmt.Errorf("no source set (validator failed?)")
	}
	return out, nil
}
