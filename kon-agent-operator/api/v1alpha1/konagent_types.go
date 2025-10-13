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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"time"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// KonAgentSpec defines the desired state of KonAgent
type KonAgentSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// +kubebuilder:validation:Required
	ClientId string `json:"clientId"`

	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	Replicas *int32 `json:"replicas,omitempty"`

	// add plugins config
	// +kubebuilder:validation:Required
	Plugins map[string]PluginConfig `json:"plugins"`

	// Cache configuration
	Cache *CacheConfig `json:"cache,omitempty"`
}

// PluginConfig defines the configuration for a specific plugin
type PluginConfig struct {
	// Enable determines whether the plugin is enabled
	// +kubebuilder:validation:Required
	Enable bool `json:"enable"`

	// Period defines how often the plugin should run
	// +kubebuilder:validation:Required
	Period time.Duration `json:"period"`

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
