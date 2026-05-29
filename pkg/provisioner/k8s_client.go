package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/kubectl/pkg/scheme"
)

// RealK8sClient implements Backend using the real Kubernetes API.
type RealK8sClient struct {
	clientset  *kubernetes.Clientset
	restConfig *rest.Config
}

// NewK8sClient creates a Kubernetes client from kubeconfig or in-cluster config.
func NewK8sClient(kubeconfigPath string) (*RealK8sClient, error) {
	var config *rest.Config
	var err error

	if kubeconfigPath != "" && fileExists(kubeconfigPath) {
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	} else {
		// Try in-cluster config.
		config, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, fmt.Errorf("k8s config: %w", err)
	}

	// Allow self-signed certs for dev clusters.
	if os.Getenv("K8S_INSECURE") == "true" {
		config.TLSClientConfig.Insecure = true
	}

	// Override API server if set.
	if apiServer := os.Getenv("K8S_API_SERVER"); apiServer != "" {
		config.Host = apiServer
	}

	cs, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("k8s clientset: %w", err)
	}

	return &RealK8sClient{clientset: cs, restConfig: config}, nil
}

func (c *RealK8sClient) CreatePod(ctx context.Context, namespace string, spec SandboxSpec) error {
	pod, err := buildPod(namespace, spec)
	if err != nil {
		return err
	}
	_, err = c.clientset.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

// buildPod constructs the Pod object for a sandbox. Split out from CreatePod
// so tests can assert against it without a Kubernetes API server.
func buildPod(namespace string, spec SandboxSpec) (*corev1.Pod, error) {
	labels := copyLabels(spec.Labels)
	labels["sandbox-id"] = spec.SandboxID

	resources, err := buildResourceRequirements(spec.Resources)
	if err != nil {
		return nil, err
	}
	vols, mounts, err := buildVolumes(spec.Volumes)
	if err != nil {
		return nil, err
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "sandbox-" + spec.SandboxID,
			Namespace:   namespace,
			Labels:      labels,
			Annotations: copyAnnotations(spec.Annotations),
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:            "sandbox",
				Image:           spec.Image,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Ports: []corev1.ContainerPort{{
					Name:          "http",
					ContainerPort: 8080,
					Protocol:      corev1.ProtocolTCP,
				}},
				Env: []corev1.EnvVar{
					{Name: "SANDBOX_ID", Value: spec.SandboxID},
				},
				Resources:    resources,
				VolumeMounts: mounts,
				ReadinessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						HTTPGet: &corev1.HTTPGetAction{
							Path: "/v1/sandbox",
							Port: intstr.FromInt32(8080),
						},
					},
					InitialDelaySeconds: 5,
					PeriodSeconds:       5,
					TimeoutSeconds:      3,
					FailureThreshold:    3,
				},
				LivenessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						HTTPGet: &corev1.HTTPGetAction{
							Path: "/v1/sandbox",
							Port: intstr.FromInt32(8080),
						},
					},
					InitialDelaySeconds: 10,
					PeriodSeconds:       10,
					TimeoutSeconds:      3,
					FailureThreshold:    3,
				},
			}},
			RestartPolicy: corev1.RestartPolicyAlways,
			Volumes:       vols,
		},
	}
	if spec.RuntimeClass != "" {
		rc := spec.RuntimeClass
		pod.Spec.RuntimeClassName = &rc
	}
	return pod, nil
}

func (c *RealK8sClient) DeletePod(ctx context.Context, namespace, name string, gracePeriod int64) error {
	opts := metav1.DeleteOptions{}
	if gracePeriod > 0 {
		opts.GracePeriodSeconds = &gracePeriod
	}
	err := c.clientset.CoreV1().Pods(namespace).Delete(ctx, name, opts)
	if errors.IsNotFound(err) {
		return nil
	}
	return err
}

func (c *RealK8sClient) GetPodPhase(ctx context.Context, namespace, name string) (string, error) {
	pod, err := c.clientset.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return "NotFound", nil
		}
		return "Unknown", err
	}
	return string(pod.Status.Phase), nil
}

func (c *RealK8sClient) CreateService(ctx context.Context, namespace string, spec SandboxSpec) error {
	labels := copyLabels(spec.Labels)
	labels["sandbox-id"] = spec.SandboxID

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sandbox-" + spec.SandboxID + "-svc",
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeNodePort,
			Ports: []corev1.ServicePort{{
				Name:       "http",
				Port:       8080,
				TargetPort: intstr.FromInt32(8080),
				Protocol:   corev1.ProtocolTCP,
			}},
			Selector: map[string]string{
				"sandbox-id": spec.SandboxID,
			},
		},
	}

	_, err := c.clientset.CoreV1().Services(namespace).Create(ctx, svc, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func (c *RealK8sClient) DeleteService(ctx context.Context, namespace, name string) error {
	err := c.clientset.CoreV1().Services(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if errors.IsNotFound(err) {
		return nil
	}
	return err
}

func (c *RealK8sClient) GetNodePort(ctx context.Context, namespace, name string) (int32, error) {
	svc, err := c.clientset.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return 0, err
	}
	for _, port := range svc.Spec.Ports {
		if port.Name == "http" {
			return port.NodePort, nil
		}
	}
	return 0, fmt.Errorf("no http port found")
}

func (c *RealK8sClient) ListSandboxes(ctx context.Context, namespace string, labels map[string]string) ([]SandboxInfo, error) {
	selector := "app=agentry-sandbox"

	// Two parallel-ish LISTs (one round-trip each) avoid the previous
	// O(N) get-per-pod fan-out we'd need to read annotations & phase.
	pods, err := c.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return nil, err
	}
	svcs, err := c.clientset.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return nil, err
	}

	// Build sandbox-id -> NodePort map for O(1) lookup.
	ports := make(map[string]int32, len(svcs.Items))
	for _, svc := range svcs.Items {
		sid := svc.Labels["sandbox-id"]
		if sid == "" {
			continue
		}
		for _, port := range svc.Spec.Ports {
			if port.Name == "http" {
				ports[sid] = port.NodePort
				break
			}
		}
	}

	nodeHost := envOr("NODE_HOST", "localhost")
	result := make([]SandboxInfo, 0, len(pods.Items))
	for i := range pods.Items {
		pod := &pods.Items[i]
		sid := pod.Labels["sandbox-id"]
		if sid == "" {
			continue
		}
		nodePort := ports[sid]
		if nodePort == 0 {
			continue
		}
		result = append(result, SandboxInfo{
			SandboxID:  sid,
			SandboxURL: fmt.Sprintf("http://%s:%d", nodeHost, nodePort),
			Status:     string(pod.Status.Phase),
			ExpiresAt:  pod.Annotations[AnnotationExpiresAt],
		})
	}
	return result, nil
}

func (c *RealK8sClient) GetPodAnnotations(ctx context.Context, namespace, name string) (map[string]string, error) {
	pod, err := c.clientset.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if pod.Annotations == nil {
		return map[string]string{}, nil
	}
	return pod.Annotations, nil
}

func (c *RealK8sClient) SetPodAnnotations(ctx context.Context, namespace, name string, annotations map[string]string) error {
	if len(annotations) == 0 {
		return nil
	}
	patch := map[string]any{
		"metadata": map[string]any{
			"annotations": annotations,
		},
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	_, err = c.clientset.CoreV1().Pods(namespace).Patch(
		ctx, name, types.StrategicMergePatchType, body, metav1.PatchOptions{},
	)
	return err
}

func (c *RealK8sClient) ExecInPod(ctx context.Context, namespace, pod string, command []string) (string, error) {
	req := c.clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Command: command,
			Stdout:  true,
			Stderr:  true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(c.restConfig, "POST", req.URL())
	if err != nil {
		return "", err
	}

	var stdout, stderr safeBuffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		return "", fmt.Errorf("%v: %s", err, stderr.String())
	}
	return stdout.String(), nil
}

// ── helpers ───────────────────────────────────────────────────────────────

func copyLabels(src map[string]string) map[string]string {
	dst := make(map[string]string, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// copyAnnotations returns a fresh map copy, or nil for an empty input. The
// nil return matters: Kubernetes serializes a non-nil empty map as `{}`,
// which churns the API server etcd write for no reason.
func copyAnnotations(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]string, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func fileExists(path string) bool {
	abs, _ := filepath.Abs(path)
	info, err := os.Stat(abs)
	return err == nil && !info.IsDir()
}

// safeBuffer is a simple bytes.Buffer wrapper for exec output.
type safeBuffer struct {
	data []byte
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)
	return len(p), nil
}

func (b *safeBuffer) String() string {
	return string(b.data)
}
