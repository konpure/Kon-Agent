package controllers

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/konpure/kon-agent-operator/api/v1alpha1"
)

func sampleAgent() *corev1alpha1.KonAgent {
	return &corev1alpha1.KonAgent{
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent", Namespace: "default"},
		Spec: corev1alpha1.KonAgentSpec{
			ClientId: "edge-agent-1",
			Server:   "quic://vm:7843",
			Codec:    "gorilla",
			Plugins: map[string]corev1alpha1.PluginConfig{
				"cpu":  {Enable: true, Period: "5s"},
				"disk": {Enable: false, Period: "5s"},
			},
			WAL: &corev1alpha1.WALConfig{
				Enable:        true,
				Dir:           "/tmp/edge_agent.cache/wal",
				SegmentSize:   "16MB",
				MaxSize:       "128MB",
				FsyncInterval: "1s",
			},
			Sampling: &corev1alpha1.SamplingConfig{
				Mode:           "auto",
				SummaryWindow:  "1m",
				DegradedWindow: "30s",
			},
			Cache: &corev1alpha1.CacheConfig{
				Path:    "/tmp/edge_agent.cache",
				MaxSize: "50MB",
			},
			NodeSelector: map[string]string{"node-role.kubernetes.io/edge": "true"},
		},
	}
}

// TestGenerateConfigContent verifies the rendered agent.yaml round-trips
// through yaml and carries every configured section.
func TestGenerateConfigContent(t *testing.T) {
	r := &KonAgentReconciler{}
	out, err := r.generateConfigContent(sampleAgent())
	if err != nil {
		t.Fatalf("generateConfigContent: %v", err)
	}

	var parsed map[string]interface{}
	if err := yaml.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("generated content is not valid yaml: %v", err)
	}

	if parsed["server"] != "quic://vm:7843" {
		t.Errorf("server mismatch: %v", parsed["server"])
	}
	if parsed["client-id"] != "edge-agent-1" {
		t.Errorf("client-id mismatch: %v", parsed["client-id"])
	}
	if parsed["codec"] != "gorilla" {
		t.Errorf("codec mismatch: %v", parsed["codec"])
	}

	plugins, ok := parsed["plugins"].(map[string]interface{})
	if !ok {
		t.Fatalf("plugins section missing or wrong type")
	}
	cpu, ok := plugins["cpu"].(map[string]interface{})
	if !ok || cpu["enable"] != true || cpu["period"] != "5s" {
		t.Errorf("cpu plugin entry wrong: %v", plugins["cpu"])
	}

	wal, ok := parsed["wal"].(map[string]interface{})
	if !ok || wal["enable"] != true || wal["max_size"] != "128MB" {
		t.Errorf("wal section wrong: %v", parsed["wal"])
	}
	sampling, ok := parsed["sampling"].(map[string]interface{})
	if !ok || sampling["degraded_window"] != "30s" {
		t.Errorf("sampling section wrong: %v", parsed["sampling"])
	}
	cache, ok := parsed["cache"].(map[string]interface{})
	if !ok || cache["max_size"] != "50MB" {
		t.Errorf("cache section wrong: %v", parsed["cache"])
	}

	// The old hand-rolled generator's bug signature must never come back.
	if strings.Contains(out, "%!d(") {
		t.Errorf("formatting artifact in output: %s", out)
	}
}

// TestBuildDaemonSet asserts the eBPF agent's required pod shape.
func TestBuildDaemonSet(t *testing.T) {
	r := &KonAgentReconciler{}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent-config"},
		Data:       map[string]string{"agent.yaml": "server: quic://vm:7843\n"},
	}

	ds := r.buildDaemonSet(sampleAgent(), cm)

	if ds.Name != "test-agent-agent" {
		t.Errorf("daemonset name mismatch: %s", ds.Name)
	}
	if !ds.Spec.Template.Spec.HostNetwork {
		t.Error("hostNetwork must be true (XDP attaches to the host NIC)")
	}

	if len(ds.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(ds.Spec.Template.Spec.Containers))
	}
	c := ds.Spec.Template.Spec.Containers[0]
	if c.SecurityContext == nil || c.SecurityContext.Privileged == nil || !*c.SecurityContext.Privileged {
		t.Error("container must run privileged for eBPF attach")
	}
	if c.Image != "konpure/kon-agent:latest" {
		t.Errorf("default image mismatch: %s", c.Image)
	}

	// Host mounts: /proc, /sys, tracefs, cache.
	mounts := map[string]string{}
	for _, m := range c.VolumeMounts {
		mounts[m.Name] = m.MountPath
	}
	for name, want := range map[string]string{
		"proc":    "/host/proc",
		"sys":     "/host/sys",
		"tracefs": "/sys/kernel/tracing",
	} {
		if mounts[name] != want {
			t.Errorf("mount %s: got %q, want %q", name, mounts[name], want)
		}
	}

	// Config hash annotation must be present and change with content.
	hash1 := ds.Spec.Template.Annotations["konpure.com/config-hash"]
	if hash1 == "" {
		t.Fatal("config-hash annotation missing")
	}
	cm.Data["agent.yaml"] = "server: quic://other:7843\n"
	hash2 := r.buildDaemonSet(sampleAgent(), cm).Spec.Template.Annotations["konpure.com/config-hash"]
	if hash1 == hash2 {
		t.Error("config-hash must change when config content changes")
	}

	// nodeSelector passes through.
	if ds.Spec.Template.Spec.NodeSelector["node-role.kubernetes.io/edge"] != "true" {
		t.Errorf("nodeSelector not propagated: %v", ds.Spec.Template.Spec.NodeSelector)
	}
}

// TestBuildDaemonSetImageOverride covers the spec.image field.
func TestBuildDaemonSetImageOverride(t *testing.T) {
	r := &KonAgentReconciler{}
	agent := sampleAgent()
	agent.Spec.Image = "registry.example.com/kon-agent:v1"
	cm := &corev1.ConfigMap{Data: map[string]string{"agent.yaml": ""}}

	ds := r.buildDaemonSet(agent, cm)
	if ds.Spec.Template.Spec.Containers[0].Image != "registry.example.com/kon-agent:v1" {
		t.Errorf("image override ignored: %s", ds.Spec.Template.Spec.Containers[0].Image)
	}
}

// TestActivePluginNames verifies spec-derived plugin listing.
func TestActivePluginNames(t *testing.T) {
	names := activePluginNames(&sampleAgent().Spec)
	if len(names) != 1 || names[0] != "cpu" {
		t.Errorf("expected [cpu], got %v", names)
	}
}
