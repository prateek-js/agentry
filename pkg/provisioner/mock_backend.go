package provisioner

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// MockBackend is an in-memory Backend implementation for tests.
//
// It is safe for concurrent use. All methods are deterministic so tests can
// assert exact call sequences via the recorded fields.
type MockBackend struct {
	mu       sync.Mutex
	pods     map[string]podRecord
	services map[string]int32 // svc name -> assigned NodePort
	nextPort int32

	// CreatePodErr, when non-nil, is returned from CreatePod for the next
	// call only and then cleared. Useful for fault-injection tests.
	CreatePodErr error
}

type podRecord struct {
	spec        SandboxSpec
	phase       string
	annotations map[string]string
}

// NewMockBackend returns a ready-to-use mock with no pods.
func NewMockBackend() *MockBackend {
	return &MockBackend{
		pods:     make(map[string]podRecord),
		services: make(map[string]int32),
		nextPort: 30000,
	}
}

func (m *MockBackend) CreatePod(_ context.Context, _ string, spec SandboxSpec) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.CreatePodErr != nil {
		err := m.CreatePodErr
		m.CreatePodErr = nil
		return err
	}
	name := "sandbox-" + spec.SandboxID
	if _, ok := m.pods[name]; ok {
		return errors.New("already exists")
	}
	ann := make(map[string]string, len(spec.Annotations))
	for k, v := range spec.Annotations {
		ann[k] = v
	}
	m.pods[name] = podRecord{spec: spec, phase: "Running", annotations: ann}
	return nil
}

func (m *MockBackend) DeletePod(_ context.Context, _, name string, _ int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.pods[name]; !ok {
		return errors.New("not found")
	}
	delete(m.pods, name)
	return nil
}

func (m *MockBackend) GetPodPhase(_ context.Context, _, name string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pods[name]
	if !ok {
		return "NotFound", nil
	}
	return p.phase, nil
}

func (m *MockBackend) CreateService(_ context.Context, _ string, spec SandboxSpec) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	name := "sandbox-" + spec.SandboxID + "-svc"
	if _, ok := m.services[name]; ok {
		return errors.New("already exists")
	}
	m.services[name] = m.nextPort
	m.nextPort++
	return nil
}

func (m *MockBackend) DeleteService(_ context.Context, _, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.services[name]; !ok {
		return errors.New("not found")
	}
	delete(m.services, name)
	return nil
}

func (m *MockBackend) GetNodePort(_ context.Context, _, name string) (int32, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.services[name]; ok {
		return p, nil
	}
	return 0, fmt.Errorf("service %s not found", name)
}

func (m *MockBackend) ListSandboxes(_ context.Context, _ string, _ map[string]string) ([]SandboxInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SandboxInfo, 0, len(m.pods))
	for name, p := range m.pods {
		svcName := name + "-svc"
		port := m.services[svcName]
		if port == 0 {
			continue
		}
		out = append(out, SandboxInfo{
			SandboxID:  p.spec.SandboxID,
			SandboxURL: fmt.Sprintf("http://%s:%d", p.spec.NodeHost, port),
			Status:     p.phase,
			ExpiresAt:  p.annotations[AnnotationExpiresAt],
		})
	}
	return out, nil
}

func (m *MockBackend) GetPodAnnotations(_ context.Context, _, name string) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pods[name]
	if !ok {
		return nil, fmt.Errorf("pod %s not found", name)
	}
	out := make(map[string]string, len(p.annotations))
	for k, v := range p.annotations {
		out[k] = v
	}
	return out, nil
}

func (m *MockBackend) SetPodAnnotations(_ context.Context, _, name string, annotations map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pods[name]
	if !ok {
		return fmt.Errorf("pod %s not found", name)
	}
	if p.annotations == nil {
		p.annotations = make(map[string]string, len(annotations))
	}
	for k, v := range annotations {
		p.annotations[k] = v
	}
	m.pods[name] = p
	return nil
}

// SetPodPhase is a test helper to drive phase transitions deterministically.
func (m *MockBackend) SetPodPhase(name, phase string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.pods[name]; ok {
		p.phase = phase
		m.pods[name] = p
	}
}

// SetAnnotationDirect is a test helper to seed annotations without going
// through the public API (e.g., simulate an already-expired sandbox).
func (m *MockBackend) SetAnnotationDirect(name, key, value string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pods[name]
	if !ok {
		return
	}
	if p.annotations == nil {
		p.annotations = map[string]string{}
	}
	p.annotations[key] = value
	m.pods[name] = p
}

// preSeed registers a sandbox + service at a given host:port without
// going through CreatePod/CreateService. Useful for tests that need
// the URL-lookup path to find a sandbox but don't want to go through
// the create handler.
func (m *MockBackend) preSeed(id, host string, port int32) {
	m.mu.Lock()
	defer m.mu.Unlock()
	name := "sandbox-" + id
	m.pods[name] = podRecord{
		spec:        SandboxSpec{SandboxID: id, NodeHost: host},
		phase:       "Running",
		annotations: map[string]string{},
	}
	m.services[name+"-svc"] = port
}

func (m *MockBackend) ExecInPod(_ context.Context, _, _ string, _ []string) (string, error) {
	return "", nil
}

// PodCount returns the current number of tracked pods. Test helper.
func (m *MockBackend) PodCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.pods)
}

// Spec returns the recorded spec for a pod, or false if absent.
func (m *MockBackend) Spec(podName string) (SandboxSpec, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pods[podName]
	if !ok {
		return SandboxSpec{}, false
	}
	return p.spec, true
}
