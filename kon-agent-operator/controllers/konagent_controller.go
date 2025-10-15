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
	"fmt"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"reflect"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/konpure/Kon-Agent/api/v1alpha1"
)

// KonAgentReconciler reconciles a KonAgent object
type KonAgentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=core.konpure.com,resources=konagents,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core.konpure.com,resources=konagents/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=core.konpure.com,resources=konagents/finalizers,verbs=update
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
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

	// Ensure Deployment exists
	deployment, err := r.ensureDeployment(ctx, &konAgent, configMap)
	if err != nil {
		log.Error(err, "Failed to ensure Deployment")
		r.updateStatus(ctx, &konAgent, metav1.Condition{
			Type:    "DeploymentReady",
			Status:  metav1.ConditionFalse,
			Message: err.Error(),
			Reason:  "DeploymentCreationFailed"})
		return ctrl.Result{Requeue: true, RequeueAfter: 5 * time.Second}, err
	}

	// Check if Deployment is ready
	if deployment.Status.AvailableReplicas > 0 && deployment.Status.ReadyReplicas == deployment.Status.AvailableReplicas {
		// Update DeploymentReady status to true
		r.updateStatus(ctx, &konAgent, metav1.Condition{
			Type:    "DeploymentReady",
			Status:  metav1.ConditionTrue,
			Message: "Deployment is ready",
			Reason:  "DeploymentReady"})

		// Update the observed generation to indicate that we've processed this spec version
		konAgent.Status.ObservedGeneration = konAgent.Generation
	} else {
		r.updateStatus(ctx, &konAgent, metav1.Condition{
			Type:    "DeploymentReady",
			Status:  metav1.ConditionFalse,
			Message: fmt.Sprintf("Only %d out of %d replicas are ready", deployment.Status.ReadyReplicas, deployment.Status.AvailableReplicas),
			Reason:  "DeploymentNotReady"})
		return ctrl.Result{Requeue: true, RequeueAfter: 2 * time.Second}, nil
	}

	// Update KonAgent status
	if err := r.Status().Update(ctx, &konAgent); err != nil {
		log.Error(err, "Failed to update KonAgent status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *KonAgentReconciler) buildDeployment(konAgent *corev1alpha1.KonAgent, configMap *corev1.ConfigMap) *appsv1.Deployment {
	// Define the desired Deployment
	replicas := int32(1)
	if konAgent.Spec.Replicas != nil {
		replicas = *konAgent.Spec.Replicas
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-deployment", konAgent.Name),
			Namespace: konAgent.Namespace,
			Labels: map[string]string{
				"app":      "kon-agent",
				"instance": konAgent.Name,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
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
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "kon-agent",
							Image: "konpure/kon-agent:latest",
							Command: []string{
								"./edge-agent",
							},
							Args: []string{
								fmt.Sprintf("--config=%s", "/etc/kon-agent/agent.yaml"),
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "config-volume",
									MountPath: "/etc/kon-agent",
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
					},
				},
			},
		},
	}
}

func (r *KonAgentReconciler) ensureDeployment(ctx context.Context, konAgent *corev1alpha1.KonAgent, configMap *corev1.ConfigMap) (*appsv1.Deployment, error) {
	log := log.FromContext(ctx)

	deploymentName := fmt.Sprintf("%s-deployment", konAgent.Name)
	namespace := konAgent.Namespace

	// Check if Deployment exists
	var existingDeployment appsv1.Deployment
	err := r.Get(ctx, types.NamespacedName{
		Name:      deploymentName,
		Namespace: namespace,
	}, &existingDeployment)

	// Define the desired Deployment
	desiredDeploy := r.buildDeployment(konAgent, configMap)

	if err != nil {
		if errors.IsNotFound(err) {
			// Create new Deployment
			if err := ctrl.SetControllerReference(konAgent, desiredDeploy, r.Scheme); err != nil {
				return nil, err
			}

			if err := r.Create(ctx, desiredDeploy); err != nil {
				log.Error(err, "Failed to create Deployment")
				return nil, err
			}
			log.Info("Created Deployment", "name", desiredDeploy.Name, "namespace", desiredDeploy.Namespace)
			return desiredDeploy, nil
		}
		return nil, err
	}
	// Update existing Deployment if needed
	needsUpdate := false

	// Check if replicas has changed with nil pointer check
	existingReplicas := int32(1)
	desiredReplicas := int32(1)

	if existingDeployment.Spec.Replicas != nil {
		existingReplicas = *existingDeployment.Spec.Replicas
	}
	if desiredDeploy.Spec.Replicas != nil {
		desiredReplicas = *desiredDeploy.Spec.Replicas
	}

	if existingReplicas != desiredReplicas {
		existingDeployment.Spec.Replicas = &desiredReplicas
		needsUpdate = true
	}

	// Check if pod template has changed
	if !r.podTemplatesEqual(existingDeployment.Spec.Template, desiredDeploy.Spec.Template) {
		existingDeployment.Spec.Template = desiredDeploy.Spec.Template
		needsUpdate = true
	}

	if needsUpdate {
		if err := r.Update(ctx, &existingDeployment); err != nil {
			log.Error(err, "Failed to update Deployment")
			return nil, err
		}
		log.Info("Updated Deployment", "name", existingDeployment.Name, "namespace", existingDeployment.Namespace)
	}

	return &existingDeployment, nil
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
	configContent := r.generateConfigContent(konAgent)

	// Check if ConfigMap exists
	var existingCM corev1.ConfigMap
	err := r.Get(ctx, types.NamespacedName{
		Name:      configMapName,
		Namespace: namespace,
	}, &existingCM)

	if err != nil {
		if errors.IsNotFound(err) {
			// Create new ComfigMap
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

func (r *KonAgentReconciler) generateConfigContent(konAgent *corev1alpha1.KonAgent) string {
	// This function generates the agent.yaml content based on the KonAgent spec
	// You'll need to implement this according to your configuration format
	// This is a simplified example
	config := fmt.Sprintf("client-id: %s\n", konAgent.Spec.ClientId)
	config += "plugins:\n"
	for name, plugin := range konAgent.Spec.Plugins {
		config += fmt.Sprintf("  %s:\n", name)
		config += fmt.Sprintf("    enable: %v\n", plugin.Enable)
		config += fmt.Sprintf("    period: %d\n", plugin.Period)
		if plugin.PluginType != "" {
			config += fmt.Sprintf("    type: %s\n", plugin.PluginType)
		}
	}
	if konAgent.Spec.Cache != nil {
		config += "cache:\n"
		config += fmt.Sprintf("  path: %s\n", konAgent.Spec.Cache.Path)
		config += fmt.Sprintf("  max-size: %d\n", konAgent.Spec.Cache.MaxSize)
	}
	return config
}

// SetupWithManager sets up the controller with the Manager.
func (r *KonAgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.KonAgent{}).
		Owns(&appsv1.Deployment{}).
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

	return nil
}
