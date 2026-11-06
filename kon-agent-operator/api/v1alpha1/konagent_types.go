/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// KonAgentSpec defines the desired state of KonAgent
type KonAgentSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// +kubebuilder:validation:Required
	ClientId string `json:"clientId"`

	// Server is the ingest endpoint the agent reports to, e.g. "quic://vm:7843".
	// Without it the generated agent.yaml has nowhere to connect.
	// +kubebuilder:validation:Required
	Server string `json:"server"`

	// Image is the agent container image; defaults to konpure/kon-agent:latest.
	Image string `json:"image,omitempty"`

	// NodeSelector limits the nodes the agent DaemonSet schedules onto,
	// e.g. {"node-role.kubernetes.io/edge": "true"}.
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations lets the agent pods schedule onto tainted nodes
	// (e.g. dedicated edge nodes).
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// add plugins config
	// +kubebuilder:validation:Required
	Plugins map[string]PluginConfig `json:"plugins"`

	// Codec selects the wire encoding: gorilla | snappy | raw
	// (defaults to gorilla on the agent).
	Codec string `json:"codec,omitempty"`

	// WAL configures the write-ahead log; mirrors the agent's wal section.
	WAL *WALConfig `json:"wal,omitempty"`

	// Sampling configures downsampling; mirrors the agent's sampling section.
	Sampling *SamplingConfig `json:"sampling,omitempty"`

	// Cache configuration
	Cache *CacheConfig `json:"cache,omitempty"`
}

// PluginConfig defines the configuration for a specific plugin
type PluginConfig struct {
	// Enable determines whether the plugin is enabled
	// +kubebuilder:validation:Required
	Enable bool `json:"enable"`

	// Period defines how often the plugin should run, e.g. "5s".
	// A string is used instead of time.Duration because the CRD schema
	// serializes Duration as raw nanoseconds, which is unreadable in a CR.
	// +kubebuilder:validation:Required
	Period string `json:"period"`

	// PluginType specifies the type of the plugin
	PluginType string `json:"pluginType,omitempty"`
}

// CacheConfig defines the cache configuration for the agent
type CacheConfig struct {
	// Path where the cache file is stored
	Path string `json:"path,omitempty"`

	// MaxSize defines the maximum size of the cache
	MaxSize string `json:"maxSize,omitempty"`
}

// WALConfig mirrors the agent's write-ahead log configuration.
type WALConfig struct {
	// Enable toggles the WAL (durable queue between pipeline and exporter)
	// +kubebuilder:validation:Required
	Enable bool `json:"enable"`

	// Dir stores the WAL segments; defaults to <cache path>/wal
	Dir string `json:"dir,omitempty"`

	// SegmentSize per WAL segment file, e.g. "16MB"
	SegmentSize string `json:"segmentSize,omitempty"`

	// MaxSize caps total WAL disk usage, e.g. "128MB"
	MaxSize string `json:"maxSize,omitempty"`

	// FsyncInterval between fsyncs, e.g. "1s"
	FsyncInterval string `json:"fsyncInterval,omitempty"`
}

// SamplingConfig mirrors the agent's downsampling configuration.
type SamplingConfig struct {
	// Mode: off | auto (degrade on WAL backpressure) | always (weak-network summary)
	Mode string `json:"mode,omitempty"`

	// SummaryWindow for mode=always, e.g. "1m"
	SummaryWindow string `json:"summaryWindow,omitempty"`

	// DegradedWindow under WAL backpressure, e.g. "30s"
	DegradedWindow string `json:"degradedWindow,omitempty"`
}

// KonAgentStatus defines the observed state of KonAgent
type KonAgentStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// ObservedGeneration is the most recent generation observed for this resource by the controller.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions represent the latest available observations of an object's state
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ActivePlugins lists the plugins currently running
	ActivePlugins []string `json:"activePlugins,omitempty"`

	// LastHeartbeatTime is the last time the agent sent a heartbeat
	LastHeartbeatTime metav1.Time `json:"lastHeartbeatTime,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// KonAgent is the Schema for the konagents API
type KonAgent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KonAgentSpec   `json:"spec,omitempty"`
	Status KonAgentStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// KonAgentList contains a list of KonAgent
type KonAgentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KonAgent `json:"items"`
}

func init() {
	SchemeBuilder.Register(&KonAgent{}, &KonAgentList{})
}
