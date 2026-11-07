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

package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"reflect"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/konpure/kon-agent-operator/api/v1alpha1"
)

// KonAgentReconciler reconciles a KonAgent object
type KonAgentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=core.konpure.com,resources=konagents,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core.konpure.com,resources=konagents/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=core.konpure.com,resources=konagents/finalizers,verbs=update
//+kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the KonAgent object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.14.1/pkg/reconcile
func (r *KonAgentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Fetch the KonAgent instance
	var konAgent corev1alpha1.KonAgent
	if err := r.Get(ctx, req.NamespacedName, &konAgent); err != nil {
		log.Error(err, "Failed to fetch KonAgent")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log.Info("Reconciling KonAgent", "name", konAgent.Name, "namespace", konAgent.Namespace)

	// TODO(user): your logic here

	if konAgent.ObjectMeta.DeletionTimestamp.IsZero() {
		if !containsString(konAgent.GetFinalizers(), controllerFinalizer) {
			r.addFinalizer(&konAgent)
			if err := r.Update(ctx, &konAgent); err != nil {
				log.Error(err, "Failed to add finalizer")
				return ctrl.Result{}, err
			}
		}
	} else {
		// The object is being deleted
		if containsString(konAgent.GetFinalizers(), controllerFinalizer) {
			// Run finalization logic
			if err := r.finalizeKonAgent(ctx, &konAgent); err != nil {
				return ctrl.Result{}, err
			}

			// Remove finalizer
			r.removeFinalizer(&konAgent)
			if err := r.Update(ctx, &konAgent); err != nil {
				return ctrl.Result{}, err
			}
		}

		// Stop reconciliation as the item is being deleted
		return ctrl.Result{}, nil
	}

	// Ensure ConfigMap exists
	configMap, err := r.ensureConfigMap(ctx, &konAgent)
	if err != nil {
		log.Error(err, "Failed to ensure ConfigMap")
		r.updateStatus(ctx, &konAgent, metav1.Condition{
			Type:    "ConfigReady",
			Status:  metav1.ConditionFalse,
			Message: err.Error(),
			Reason:  "ConfigMapCreationFailed"})
		return ctrl.Result{Requeue: true, RequeueAfter: 5 * time.Second}, err
	}

	// Update ConfigReady status to true
	r.updateStatus(ctx, &konAgent, metav1.Condition{
		Type:    "ConfigReady",
		Status:  metav1.ConditionTrue,
		Message: "ConfigMap is ready",
		Reason:  "ConfigMapReady"})

	// Ensure DaemonSet exists
	daemonSet, err := r.ensureDaemonSet(ctx, &konAgent, configMap)
	if err != nil {
		log.Error(err, "Failed to ensure DaemonSet")
		r.updateStatus(ctx, &konAgent, metav1.Condition{
			Type:    "DaemonSetReady",
			Status:  metav1.ConditionFalse,
			Message: err.Error(),
			Reason:  "DaemonSetCreationFailed"})
		return ctrl.Result{Requeue: true, RequeueAfter: 5 * time.Second}, err
	}

	// Check if DaemonSet is ready: all desired pods scheduled and ready.
	if daemonSet.Status.DesiredNumberScheduled > 0 &&
		daemonSet.Status.NumberReady == daemonSet.Status.DesiredNumberScheduled {
		r.updateStatus(ctx, &konAgent, metav1.Condition{
			Type:    "DaemonSetReady",
			Status:  metav1.ConditionTrue,
			Message: "DaemonSet is ready",
			Reason:  "DaemonSetReady"})

		// Update the observed generation to indicate that we've processed this spec version
		konAgent.Status.ObservedGeneration = konAgent.Generation
	} else {
		r.updateStatus(ctx, &konAgent, metav1.Condition{
			Type:    "DaemonSetReady",
			Status:  metav1.ConditionFalse,
			Message: fmt.Sprintf("Only %d out of %d pods are ready", daemonSet.Status.NumberReady, daemonSet.Status.DesiredNumberScheduled),
			Reason:  "DaemonSetNotReady"})
		return ctrl.Result{Requeue: true, RequeueAfter: 2 * time.Second}, nil
	}

	// Update KonAgent status: plugins are derived from the spec (the
	// declarative source of truth); the heartbeat marks the last successful
	// reconcile.
	konAgent.Status.ActivePlugins = activePluginNames(&konAgent.Spec)
	konAgent.Status.LastHeartbeatTime = metav1.Now()

	// Update KonAgent status
	if err := r.Status().Update(ctx, &konAgent); err != nil {
		log.Error(err, "Failed to update KonAgent status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// activePluginNames returns the sorted names of enabled plugins.
func activePluginNames(spec *corev1alpha1.KonAgentSpec) []string {
	var names []string
	for name, p := range spec.Plugins {
		if p.Enable {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// buildDaemonSet renders the desired DaemonSet for one KonAgent CR.
//
// The pod spec reflects the agent's eBPF collector nature:
//   - privileged: loading BPF programs and attaching XDP/kprobes/tracepoints
//     all require it (a fine-grained CAP_BPF/CAP_SYS_ADMIN alternative is
//     noted in the docs but needs a newer kernel's BPF token model);
//   - hostNetwork: XDP attaches to the host NIC and all network metrics must
//     describe the host namespace, not the pod's;
//   - /proc and /sys hostPath mounts: the /proc plugins read system stats and
//     CO-RE relocation needs /sys/kernel/btf plus /sys/dev/block symlinks.
func (r *KonAgentReconciler) buildDaemonSet(konAgent *corev1alpha1.KonAgent, configMap *corev1.ConfigMap) *appsv1.DaemonSet {
	image := konAgent.Spec.Image
	if image == "" {
		image = "konpure/kon-agent:latest"
	}

	hostPathDir := corev1.HostPathDirectory
	hostPathDirOrCreate := corev1.HostPathDirectoryOrCreate

	// The WAL (and state file) must survive pod restarts — a container-local
	// /tmp would silently void the crash-recovery guarantee. The agent's
	// cache path is backed by a host directory.
	cachePath := "/tmp/edge_agent.cache"
	if c := konAgent.Spec.Cache; c != nil && c.Path != "" {
		cachePath = c.Path
	}

	// Rolling update trigger: when the ConfigMap content changes, the hash
	// annotation on the pod template changes, and the DaemonSet rolls.
	configHash := sha256.Sum256([]byte(configMap.Data["agent.yaml"]))

	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-agent", konAgent.Name),
			Namespace: konAgent.Namespace,
			Labels: map[string]string{
				"app":      "kon-agent",
				"instance": konAgent.Name,
			},
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app":      "kon-agent",
					"instance": konAgent.Name,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":      "kon-agent",
						"instance": konAgent.Name,
					},
					Annotations: map[string]string{
						"konpure.com/config-hash": hex.EncodeToString(configHash[:]),
					},
				},
				Spec: corev1.PodSpec{
					HostNetwork:   true,
					NodeSelector:  konAgent.Spec.NodeSelector,
					Tolerations:   konAgent.Spec.Tolerations,
					RestartPolicy: corev1.RestartPolicyAlways,
					Containers: []corev1.Container{
						{
							Name:  "kon-agent",
							Image: image,
							Command: []string{
								"./edge-agent",
							},
							Args: []string{
								fmt.Sprintf("--config=%s", "/etc/kon-agent/agent.yaml"),
							},
							Env: []corev1.EnvVar{
								// gopsutil-based plugins read the host's procfs/sysfs
								// via these (node_exporter-style convention).
								{Name: "HOST_PROC", Value: "/host/proc"},
								{Name: "HOST_SYS", Value: "/host/sys"},
							},
							SecurityContext: &corev1.SecurityContext{
								Privileged: ptr(true),
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "config-volume",
									MountPath: "/etc/kon-agent",
								},
								{
									Name:      "proc",
									MountPath: "/host/proc",
									ReadOnly:  true,
								},
								{
									Name:      "sys",
									MountPath: "/host/sys",
									ReadOnly:  true,
								},
								{
									Name:      "agent-cache",
									MountPath: cachePath,
								},
								{
									// tracepoint attach (sched/tcp/skb) goes
									// through tracefs; containers get a fresh
									// sysfs without it, so mount the host's.
									Name:      "tracefs",
									MountPath: "/sys/kernel/tracing",
									ReadOnly:  true,
								},
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									"cpu":    resource.MustParse("100m"),
									"memory": resource.MustParse("128Mi"),
								},
								Limits: corev1.ResourceList{
									"cpu":    resource.MustParse("500m"),
									"memory": resource.MustParse("256Mi"),
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "config-volume",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: configMap.Name,
									},
								},
							},
						},
						{
							Name: "proc",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/proc",
									Type: &hostPathDir,
								},
							},
						},
						{
							Name: "sys",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/sys",
									Type: &hostPathDir,
								},
							},
						},
						{
							Name: "agent-cache",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/var/lib/kon-agent",
									Type: &hostPathDirOrCreate,
								},
							},
						},
						{
							Name: "tracefs",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/sys/kernel/tracing",
									Type: &hostPathDir,
								},
							},
						},
					},
				},
			},
		},
	}
}

func ptr(b bool) *bool { return &b }

func (r *KonAgentReconciler) ensureDaemonSet(ctx context.Context, konAgent *corev1alpha1.KonAgent, configMap *corev1.ConfigMap) (*appsv1.DaemonSet, error) {
	log := log.FromContext(ctx)

	daemonSetName := fmt.Sprintf("%s-agent", konAgent.Name)
	namespace := konAgent.Namespace

	// Check if DaemonSet exists
	var existingDaemonSet appsv1.DaemonSet
	err := r.Get(ctx, types.NamespacedName{
		Name:      daemonSetName,
		Namespace: namespace,
	}, &existingDaemonSet)

	// Define the desired DaemonSet
	desiredDaemonSet := r.buildDaemonSet(konAgent, configMap)

	if err != nil {
		if errors.IsNotFound(err) {
			// Create new DaemonSet
			if err := ctrl.SetControllerReference(konAgent, desiredDaemonSet, r.Scheme); err != nil {
				return nil, err
			}

			if err := r.Create(ctx, desiredDaemonSet); err != nil {
				log.Error(err, "Failed to create DaemonSet")
				return nil, err
			}
			log.Info("Created DaemonSet", "name", desiredDaemonSet.Name, "namespace", desiredDaemonSet.Namespace)
			return desiredDaemonSet, nil
		}
		return nil, err
	}

	// Update existing DaemonSet if the pod template has changed
	// (DaemonSet has no replica count to reconcile — every node gets one pod).
	if !r.podTemplatesEqual(existingDaemonSet.Spec.Template, desiredDaemonSet.Spec.Template) {
		existingDaemonSet.Spec.Template = desiredDaemonSet.Spec.Template
		if err := r.Update(ctx, &existingDaemonSet); err != nil {
			log.Error(err, "Failed to update DaemonSet")
			return nil, err
		}
		log.Info("Updated DaemonSet", "name", existingDaemonSet.Name, "namespace", existingDaemonSet.Namespace)
	}

	return &existingDaemonSet, nil
}

func (r *KonAgentReconciler) podTemplatesEqual(existing, desired corev1.PodTemplateSpec) bool {
	// Compare labels
	if !reflect.DeepEqual(existing.Labels, desired.Labels) {
		return false
	}

	// Compare annotations
	if !reflect.DeepEqual(existing.Annotations, desired.Annotations) {
		return false
	}

	// Compare container count
	if len(existing.Spec.Containers) != len(desired.Spec.Containers) {
		return false
	}

	// Compare each container's critical attributes
	for i := range existing.Spec.Containers {
		existingContainer := existing.Spec.Containers[i]
		desiredContainer := desired.Spec.Containers[i]

		if existingContainer.Name != desiredContainer.Name ||
			existingContainer.Image != desiredContainer.Image ||
			!reflect.DeepEqual(existingContainer.Command, desiredContainer.Command) ||
			!reflect.DeepEqual(existingContainer.Args, desiredContainer.Args) ||
			!reflect.DeepEqual(existingContainer.VolumeMounts, desiredContainer.VolumeMounts) {
			return false
		}
	}

	// Compare volumes
	if !reflect.DeepEqual(existing.Spec.Volumes, desired.Spec.Volumes) {
		return false
	}

	return true
}

func (r *KonAgentReconciler) ensureConfigMap(ctx context.Context, konAgent *corev1alpha1.KonAgent) (*corev1.ConfigMap, error) {
	log := log.FromContext(ctx)
	configMapName := fmt.Sprintf("%s-config", konAgent.Name)
	namespace := konAgent.Namespace

	// Generate agent.yaml content
	configContent, err := r.generateConfigContent(konAgent)
	if err != nil {
		return nil, err
	}

	// Check if ConfigMap exists
	var existingCM corev1.ConfigMap
	err = r.Get(ctx, types.NamespacedName{
		Name:      configMapName,
		Namespace: namespace,
	}, &existingCM)

	if err != nil {
		if errors.IsNotFound(err) {
			// Create new ConfigMap
			configMap := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      configMapName,
					Namespace: namespace,
					Labels: map[string]string{
						"app":      "kon-agent",
						"instance": konAgent.Name,
					},
				},
				Data: map[string]string{
					"agent.yaml": configContent,
				},
			}

			// Set owner reference
			if err := ctrl.SetControllerReference(konAgent, configMap, r.Scheme); err != nil {
				log.Error(err, "Failed to set controller reference")
				return nil, err
			}

			if err := r.Create(ctx, configMap); err != nil {
				log.Error(err, "Failed to create ConfigMap")
				return nil, err
			}

			log.Info("ConfigMap created", "name", configMap.Name, "namespace", configMap.Namespace)
			return configMap, nil
		}
		return nil, err
	}

	// Update existing ConfigMap if content has changed
	if existingCM.Data["agent.yaml"] != configContent {
		existingCM.Data["agent.yaml"] = configContent
		if err := r.Update(ctx, &existingCM); err != nil {
			log.Error(err, "Failed to update ConfigMap")
			return nil, err
		}
		log.Info("Updated ConfigMap", "name", existingCM.Name)
	}

	return &existingCM, nil
}

// agentConfig mirrors the agent's config loader schema; yaml tags must match
// it exactly (kebab-case client-id, snake_case max_size, etc).
type agentConfig struct {
	Server   string                 `yaml:"server"`
	ClientID string                 `yaml:"client-id"`
	Codec    string                 `yaml:"codec,omitempty"`
	Plugins  map[string]pluginEntry `yaml:"plugins"`
	Cache    *cacheSection          `yaml:"cache,omitempty"`
	WAL      *walSection            `yaml:"wal,omitempty"`
	Sampling *samplingSection       `yaml:"sampling,omitempty"`
}

type pluginEntry struct {
	Enable bool   `yaml:"enable"`
	Period string `yaml:"period"`
	Type   string `yaml:"type,omitempty"`
}

type cacheSection struct {
	Path    string `yaml:"path"`
	MaxSize string `yaml:"max_size"`
}

type walSection struct {
	Enable        bool   `yaml:"enable"`
	Dir           string `yaml:"dir,omitempty"`
	SegmentSize   string `yaml:"segment_size,omitempty"`
	MaxSize       string `yaml:"max_size,omitempty"`
	FsyncInterval string `yaml:"fsync_interval,omitempty"`
}

type samplingSection struct {
	Mode           string `yaml:"mode,omitempty"`
	SummaryWindow  string `yaml:"summary_window,omitempty"`
	DegradedWindow string `yaml:"degraded_window,omitempty"`
}

// generateConfigContent renders agent.yaml from the KonAgent spec by
// marshaling a structured value — no hand-rolled string concatenation, so
// key/format bugs of the previous implementation cannot reoccur.
func (r *KonAgentReconciler) generateConfigContent(konAgent *corev1alpha1.KonAgent) (string, error) {
	cfg := agentConfig{
		Server:   konAgent.Spec.Server,
		ClientID: konAgent.Spec.ClientId,
		Codec:    konAgent.Spec.Codec,
		Plugins:  make(map[string]pluginEntry, len(konAgent.Spec.Plugins)),
	}
	for name, p := range konAgent.Spec.Plugins {
		cfg.Plugins[name] = pluginEntry{
			Enable: p.Enable,
			Period: p.Period,
			Type:   p.PluginType,
		}
	}
	if c := konAgent.Spec.Cache; c != nil {
		cfg.Cache = &cacheSection{Path: c.Path, MaxSize: c.MaxSize}
	}
	if w := konAgent.Spec.WAL; w != nil {
		cfg.WAL = &walSection{
			Enable:        w.Enable,
			Dir:           w.Dir,
			SegmentSize:   w.SegmentSize,
			MaxSize:       w.MaxSize,
			FsyncInterval: w.FsyncInterval,
		}
	}
	if s := konAgent.Spec.Sampling; s != nil {
		cfg.Sampling = &samplingSection{
			Mode:           s.Mode,
			SummaryWindow:  s.SummaryWindow,
			DegradedWindow: s.DegradedWindow,
		}
	}

	out, err := yaml.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("failed to marshal agent config: %w", err)
	}
	return string(out), nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *KonAgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.KonAgent{}).
		Owns(&appsv1.DaemonSet{}).
		Owns(&corev1.ConfigMap{}).
		Complete(r)
}

func (r *KonAgentReconciler) updateStatus(ctx context.Context, konAgent *corev1alpha1.KonAgent, condition metav1.Condition) error {
	// Update the status with the given condition
	log := log.FromContext(ctx)
	condition.LastTransitionTime = metav1.Now()

	found := false
	for i, c := range konAgent.Status.Conditions {
		if c.Type == condition.Type {
			if c.Status != condition.Status || c.Reason != condition.Reason {
				konAgent.Status.Conditions[i] = condition
			}
			found = true
			break
		}
	}

	if !found {
		konAgent.Status.Conditions = append(konAgent.Status.Conditions, condition)
	}

	if err := r.Status().Update(ctx, konAgent); err != nil {
		log.Error(err, "Failed to update KonAgent status")
		return err
	}
	return nil
}

const controllerFinalizer = "core.konpure.com/finalizer"

// Helper functions for finalizer
func containsString(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}

func removeString(slice []string, s string) (result []string) {
	for _, item := range slice {
		if item != s {
			result = append(result, item)
		}
	}
	return
}

func (r *KonAgentReconciler) addFinalizer(konAgent *corev1alpha1.KonAgent) {
	log := log.FromContext(context.Background())
	log.Info("Adding finalizer for KonAgent", "name", konAgent.Name)
	konAgent.SetFinalizers(append(konAgent.GetFinalizers(), controllerFinalizer))
}

func (r *KonAgentReconciler) removeFinalizer(konAgent *corev1alpha1.KonAgent) {
	log := log.FromContext(context.Background())
	log.Info("Removing finalizer for KonAgent", "name", konAgent.Name)
	konAgent.SetFinalizers(removeString(konAgent.GetFinalizers(), controllerFinalizer))
}

func (r *KonAgentReconciler) finalizeKonAgent(ctx context.Context, konAgent *corev1alpha1.KonAgent) error {
	log := log.FromContext(ctx)
	log.Info("Finalizing KonAgent", "name", konAgent.Name)

	// Here you would add any cleanup logic for your resources
	// For example, you might want to delete any additional resources created
	// outside of the owner references

	// try to delete ConfigMap
	configMapName := fmt.Sprintf("%s-config", konAgent.Name)
	configMap := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      configMapName,
		Namespace: konAgent.Namespace,
	}, configMap)
	if err == nil {
		// ConfigMap exists
		log.Info("Deleting configMap during finalization", "name", configMap.Name, "namespace", configMap.Namespace)
		if deleteerr := r.Delete(ctx, configMap); deleteerr != nil {
			log.Error(deleteerr, "Failed to delete ConfigMap", "name", configMap.Name, "namespace", configMap.Namespace)
			return deleteerr
		}
	} else if !errors.IsNotFound(err) {
		log.Error(err, "Error checking ConfigMap existence", "name", configMapName, "namespace", konAgent.Namespace)
		return err
	}

	daemonSetName := fmt.Sprintf("%s-agent", konAgent.Name)
	daemonSet := &appsv1.DaemonSet{}
	err = r.Get(ctx, types.NamespacedName{
		Name:      daemonSetName,
		Namespace: konAgent.Namespace,
	}, daemonSet)

	if err == nil {
		// DaemonSet exists
		log.Info("Deleting daemonSet during finalization", "name", daemonSet.Name, "namespace", daemonSet.Namespace)
		if deleteerr := r.Delete(ctx, daemonSet); deleteerr != nil {
			log.Error(deleteerr, "Failed to delete DaemonSet", "name", daemonSet.Name, "namespace", daemonSet.Namespace)
			return deleteerr
		}
	} else if !errors.IsNotFound(err) {
		log.Error(err, "Error checking DaemonSet existence", "name", daemonSetName, "namespace", konAgent.Namespace)
		return err
	}

	return nil
}
