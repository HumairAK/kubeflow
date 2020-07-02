/*

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
	"bytes"
	"context"
	"fmt"
	"github.com/go-logr/logr"
	reconcilehelper "github.com/kubeflow/kubeflow/components/common/reconcilehelper"
	"github.com/kubeflow/kubeflow/components/notebook-controller/api/v1beta1"
	"github.com/kubeflow/kubeflow/components/notebook-controller/pkg/culler"
	"github.com/kubeflow/kubeflow/components/notebook-controller/pkg/metrics"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apiextensions-apiserver/examples/client-go/pkg/client/clientset/versioned/scheme"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/tools/remotecommand"
	"os"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
	"strings"
)

const DefaultContainerPort = 8888
const DefaultServingPort = 80

// The default fsGroup of PodSecurityContext.
// https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.11/#podsecuritycontext-v1-core
const DefaultFSGroup = int64(100)

/*
We generally want to ignore (not requeue) NotFound errors, since we'll get a
reconciliation request once the object exists, and requeuing in the meantime
won't help.
*/
func ignoreNotFound(err error) error {
	if apierrs.IsNotFound(err) {
		return nil
	}
	return err
}

// NotebookReconciler reconciles a Notebook object
type NotebookReconciler struct {
	client.Client
	Log           logr.Logger
	Scheme        *runtime.Scheme
	Metrics       *metrics.Metrics
	EventRecorder record.EventRecorder
}

// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=statefulsets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=events,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kubeflow.org,resources=*,verbs=get;list;watch;create;update;patch;delete

func (r *NotebookReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()
	log := r.Log.WithValues("notebook", req.NamespacedName)

	// TODO(yanniszark): Can we avoid reconciling Events and Notebook in the same queue?
	// Here we are reissuing STS and POD Events as Notebook Events
	// We extract the involved NB name from the event, then record it as a Notebook event
	// Applying the same original message, but the involved object is converted to Notebook
	event := &corev1.Event{}
	var getEventErr error
	getEventErr = r.Get(ctx, req.NamespacedName, event)
	if getEventErr == nil {
		involvedNotebook := &v1beta1.Notebook{}
		nbName, err := nbNameFromInvolvedObject(r.Client, &event.InvolvedObject)
		if err != nil {
			return ctrl.Result{}, err
		}
		involvedNotebookKey := types.NamespacedName{Name: nbName, Namespace: req.Namespace}
		if err := r.Get(ctx, involvedNotebookKey, involvedNotebook); err != nil {
			log.Error(err, "unable to fetch Notebook by looking at event")
			return ctrl.Result{}, ignoreNotFound(err)
		}
		// These events
		r.EventRecorder.Eventf(involvedNotebook, event.Type, event.Reason,
			"Reissued from %s/%s: %s", strings.ToLower(event.InvolvedObject.Kind), event.InvolvedObject.Name, event.Message)
	}
	if getEventErr != nil && !apierrs.IsNotFound(getEventErr) {
		return ctrl.Result{}, getEventErr
	}
	// If not found, continue. Is not an event.

	instance := &v1beta1.Notebook{}
	if err := r.Get(ctx, req.NamespacedName, instance); err != nil {
		log.Error(err, "unable to fetch Notebook")
		return ctrl.Result{}, ignoreNotFound(err)
	}

	// Reconcile StatefulSet
	ss := generateStatefulSet(instance)
	if err := ctrl.SetControllerReference(instance, ss, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	// Check if the StatefulSet already exists
	foundStateful := &appsv1.StatefulSet{}
	justCreated := false
	err := r.Get(ctx, types.NamespacedName{Name: ss.Name, Namespace: ss.Namespace}, foundStateful)
	if err != nil && apierrs.IsNotFound(err) {
		log.Info("Creating StatefulSet", "namespace", ss.Namespace, "name", ss.Name)
		r.Metrics.NotebookCreation.WithLabelValues(ss.Namespace).Inc()
		err = r.Create(ctx, ss)
		justCreated = true
		if err != nil {
			log.Error(err, "unable to create Statefulset")
			r.Metrics.NotebookFailCreation.WithLabelValues(ss.Namespace).Inc()
			return ctrl.Result{}, err
		}
	} else if err != nil {
		log.Error(err, "error getting Statefulset")
		return ctrl.Result{}, err
	}

	// Update the foundStateful object and write the result back if there are any changes
	if !justCreated && reconcilehelper.CopyStatefulSetFields(ss, foundStateful) {
		log.Info("Updating StatefulSet", "namespace", ss.Namespace, "name", ss.Name)
		err = r.Update(ctx, foundStateful)
		if err != nil {
			log.Error(err, "unable to update Statefulset")
			return ctrl.Result{}, err
		}
	}

	// Reconcile service
	service := generateService(instance)
	if err := ctrl.SetControllerReference(instance, service, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	// Check if the Service already exists
	foundService := &corev1.Service{}
	justCreated = false
	err = r.Get(ctx, types.NamespacedName{Name: service.Name, Namespace: service.Namespace}, foundService)
	if err != nil && apierrs.IsNotFound(err) {
		log.Info("Creating Service", "namespace", service.Namespace, "name", service.Name)
		err = r.Create(ctx, service)
		justCreated = true
		if err != nil {
			log.Error(err, "unable to create Service")
			return ctrl.Result{}, err
		}
	} else if err != nil {
		log.Error(err, "error getting Statefulset")
		return ctrl.Result{}, err
	}
	// Update the foundService object and write the result back if there are any changes
	if !justCreated && reconcilehelper.CopyServiceFields(service, foundService) {
		log.Info("Updating Service\n", "namespace", service.Namespace, "name", service.Name)
		err = r.Update(ctx, foundService)
		if err != nil {
			log.Error(err, "unable to update Service")
			return ctrl.Result{}, err
		}
	}

	// Reconcile virtual service if we use ISTIO.
	if os.Getenv("USE_ISTIO") == "true" {
		err = r.reconcileVirtualService(instance)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	// Update the readyReplicas if the status is changed
	// TODO (Humair): Ignore STS reconciliation if Notebook is in maintenance
	if foundStateful.Status.ReadyReplicas != instance.Status.ReadyReplicas {
		log.Info("Updating Status", "namespace", instance.Namespace, "name", instance.Name)
		instance.Status.ReadyReplicas = foundStateful.Status.ReadyReplicas
		err = r.Status().Update(ctx, instance)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	// Here we check the Notebook pod's container state, if it has changed since we last
	// updated the Notebook CR's container state, then we update the Notebook's CR status
	// Check the pod status
	pod := &corev1.Pod{}
	podFound := false
	err = r.Get(ctx, types.NamespacedName{Name: ss.Name + "-0", Namespace: ss.Namespace}, pod)
	if err != nil && apierrs.IsNotFound(err) {
		// This should be reconciled by the StatefulSet
		log.Info("Pod not found...")
	} else if err != nil {
		return ctrl.Result{}, err
	} else {
		// Got the pod
		podFound = true
		if len(pod.Status.ContainerStatuses) > 0 &&
			pod.Status.ContainerStatuses[0].State != instance.Status.ContainerState {
			log.Info("Updating container state: ", "namespace", instance.Namespace, "name", instance.Name)
			cs := pod.Status.ContainerStatuses[0].State
			instance.Status.ContainerState = cs

			oldConditions := instance.Status.Conditions
			newCondition := getNextCondition(cs)
			// Append new condition
			if len(oldConditions) == 0 || oldConditions[0].Type != newCondition.Type ||
				oldConditions[0].Reason != newCondition.Reason ||
				oldConditions[0].Message != newCondition.Message {
				log.Info("Appending to conditions: ", "namespace", instance.Namespace, "name", instance.Name, "type", newCondition.Type, "reason", newCondition.Reason, "message", newCondition.Message)
				instance.Status.Conditions = append([]v1beta1.NotebookCondition{newCondition}, oldConditions...)
			}
			err = r.Status().Update(ctx, instance)
			if err != nil {
				return ctrl.Result{}, err
			}
		}
	}


	// TODO (Humair): Check if Pod crashed, if it did scaledown sts

	// TODO (Humair): Check Scale Job
		// If job successful, remove old pvc & maintenance label

	// If Pod is found, and Volume threshold is specified, and Volume capacity is above
	// threshold then increase capacity for volumeclaim
	if podFound && instance.Spec.ScalePVC != nil {
		// We're going through all pvcs and applying this policy
		for _, volume := range pod.Spec.Volumes {
			if volume.PersistentVolumeClaim != nil {
				threshold := instance.Spec.ScalePVC.Threshold
				increment := instance.Spec.ScalePVC.Increment
				log.Info(fmt.Sprintf("Found a PVC with claimName: %s for pod with name %s:", volume.PersistentVolumeClaim.ClaimName, pod.Name))
				log.Info(fmt.Sprintf("Treshold is set at: %d and Increment is set at: %s", threshold, increment.String()))

				// Get volumeMount path
				volumeMountPath := ""
				for _, container := range instance.Spec.Template.Spec.Containers {
					for _, volumeMount := range container.VolumeMounts {
						if volumeMount.Name == volume.Name{
							volumeMountPath = volumeMount.MountPath
							break
						}
					}
				}
				if volumeMountPath == "" {
					log.Info("Could not find volumeMountPath, aborting disk usage check.")
					break
				}
				shellCommand := fmt.Sprintf("du -hs -BK %s | awk '{print $1}'", volumeMountPath)
				usedSpace, err := execCommand([]string{"sh", "-c", shellCommand}, pod, r)
				if err != nil {
					log.Info(fmt.Sprintf("Encountered error when running exec in pod %s", pod.Name))
					break
				}

				// We have the amount of used space, now let's get the PVC associated with this Pod
				pvc := &corev1.PersistentVolumeClaim{}
				err = r.Get(ctx, types.NamespacedName{Name: volume.PersistentVolumeClaim.ClaimName, Namespace: pod.Namespace}, pvc)
				if err != nil && apierrs.IsNotFound(err) {
					log.Info("PVC associated with notebook Pod not found...")
					break
				}

				// Check if the amount of free space is under threshold
				requestQuant := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
				usedSpaceQuant, err := resource.ParseQuantity(strings.TrimSpace(usedSpace )+ "i") // append "i" to convert to k8s binary SI unit
				if err != nil {
					log.Info("Could not parse used space quantity into resource quantity aborting usage check.")
					break
				}
				log.Info(fmt.Sprintf("Current storage request: %s, current free space: %s", requestQuant.String(), usedSpaceQuant.String() ))

				requestQuantInt := requestQuant.Value()
				usedSpaceQuantInt := usedSpaceQuant.Value()

				percentSpaceUsed := int((float64(usedSpaceQuantInt) / float64(requestQuantInt)) * 100)
				log.Info(fmt.Sprintf("PVC %s disk space is at %d%% capacity", pvc.Name, percentSpaceUsed))
				if percentSpaceUsed > threshold {
					// TODO (Humair): Attempt to scale pvc
					log.Info("PVC Capacity is above threshold, attempting to scale.")
					// TODO (Humair): If we can't scale PVC mark the Notebook for Maintenance
				}
			}
		}

	}

	// Check if the Notebook needs to be stopped
	if podFound && culler.NotebookNeedsCulling(instance.ObjectMeta) {
		log.Info(fmt.Sprintf(
			"Notebook %s/%s needs culling. Setting annotations",
			instance.Namespace, instance.Name))

		// Set annotations to the Notebook
		culler.SetStopAnnotation(&instance.ObjectMeta, r.Metrics)
		r.Metrics.NotebookCullingCount.WithLabelValues(instance.Namespace, instance.Name).Inc()
		err = r.Update(ctx, instance)
		if err != nil {
			return ctrl.Result{}, err
		}
	} else if podFound && !culler.StopAnnotationIsSet(instance.ObjectMeta) {
		// The Pod is either too fresh, or the idle time has passed and it has
		// received traffic. In this case we will be periodically checking if
		// it needs culling.
		return ctrl.Result{RequeueAfter: culler.GetRequeueTime()}, nil
	}

	return ctrl.Result{}, nil
}

func execCommand(command []string, pod *corev1.Pod, r *NotebookReconciler) (string, error){
	cfg, err := config.GetConfig()
	if err != nil {
		return "", err
	}

	restClient, err := apiutil.RESTClientForGVK(pod.GroupVersionKind(), cfg, scheme.Codecs)
	if err != nil {
		return "", err
	}
	execReq := restClient.Post().Resource("pods").Name(pod.Name).Namespace(pod.Namespace).SubResource("exec")
	parameterCodec := runtime.NewParameterCodec(r.Scheme)
	execReq.VersionedParams(&corev1.PodExecOptions{
		Command:   command,
		Stdin:     true,
		Stdout:    true,
		Stderr:    true,
	}, parameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(cfg, "POST", execReq.URL())
	if err != nil {
		return "", err
	}

	var stdout, stderr bytes.Buffer
	err = exec.Stream(remotecommand.StreamOptions{
		Stdin:  os.Stdin,
		Stdout: &stdout,
		Stderr: &stderr,
		Tty:    false,
	})
	if err != nil {
		return "", err
	}

	return stdout.String(), nil
}

func getNextCondition(cs corev1.ContainerState) v1beta1.NotebookCondition {
	var nbtype = ""
	var nbreason = ""
	var nbmsg = ""

	if cs.Running != nil {
		nbtype = "Running"
	} else if cs.Waiting != nil {
		nbtype = "Waiting"
		nbreason = cs.Waiting.Reason
		nbmsg = cs.Waiting.Message
	} else {
		nbtype = "Terminated"
		nbreason = cs.Terminated.Reason
		nbmsg = cs.Terminated.Reason
	}

	newCondition := v1beta1.NotebookCondition{
		Type:          nbtype,
		LastProbeTime: metav1.Now(),
		Reason:        nbreason,
		Message:       nbmsg,
	}
	return newCondition
}

func generateStatefulSet(instance *v1beta1.Notebook) *appsv1.StatefulSet {
	replicas := int32(1)
	if culler.StopAnnotationIsSet(instance.ObjectMeta) {
		replicas = 0
	}

	ss := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instance.Name,
			Namespace: instance.Namespace,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"statefulset": instance.Name,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
					"statefulset":   instance.Name,
					"notebook-name": instance.Name,
				}},
				Spec: instance.Spec.Template.Spec,
			},
		},
	}
	// copy all of the Notebook labels to the pod including poddefault related labels
	l := &ss.Spec.Template.ObjectMeta.Labels
	for k, v := range instance.ObjectMeta.Labels {
		(*l)[k] = v
	}

	podSpec := &ss.Spec.Template.Spec
	container := &podSpec.Containers[0]
	if container.WorkingDir == "" {
		container.WorkingDir = "/home/jovyan"
	}
	if container.Ports == nil {
		container.Ports = []corev1.ContainerPort{
			{
				ContainerPort: DefaultContainerPort,
				Name:          "notebook-port",
				Protocol:      "TCP",
			},
		}
	}
	container.Env = append(container.Env, corev1.EnvVar{
		Name:  "NB_PREFIX",
		Value: "/notebook/" + instance.Namespace + "/" + instance.Name,
	})

	// For some platforms (like OpenShift), adding fsGroup: 100 is troublesome.
	// This allows for those platforms to bypass the automatic addition of the fsGroup
	// and will allow for the Pod Security Policy controller to make an appropriate choice
	// https://github.com/kubernetes-sigs/controller-runtime/issues/4617
	if value, exists := os.LookupEnv("ADD_FSGROUP"); !exists || value == "true" {
		if podSpec.SecurityContext == nil {
			fsGroup := DefaultFSGroup
			podSpec.SecurityContext = &corev1.PodSecurityContext{
				FSGroup: &fsGroup,
			}
		}
	}
	return ss
}

func generateService(instance *v1beta1.Notebook) *corev1.Service {
	// Define the desired Service object
	port := DefaultContainerPort
	containerPorts := instance.Spec.Template.Spec.Containers[0].Ports
	if containerPorts != nil {
		port = int(containerPorts[0].ContainerPort)
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instance.Name,
			Namespace: instance.Namespace,
		},
		Spec: corev1.ServiceSpec{
			Type:     "ClusterIP",
			Selector: map[string]string{"statefulset": instance.Name},
			Ports: []corev1.ServicePort{
				{
					// Make port name follow Istio pattern so it can be managed by istio rbac
					Name:       "http-" + instance.Name,
					Port:       DefaultServingPort,
					TargetPort: intstr.FromInt(port),
					Protocol:   "TCP",
				},
			},
		},
	}
	return svc
}

func virtualServiceName(kfName string, namespace string) string {
	return fmt.Sprintf("notebook-%s-%s", namespace, kfName)
}

func generateVirtualService(instance *v1beta1.Notebook) (*unstructured.Unstructured, error) {
	name := instance.Name
	namespace := instance.Namespace
	prefix := fmt.Sprintf("/notebook/%s/%s/", namespace, name)
	rewrite := fmt.Sprintf("/notebook/%s/%s/", namespace, name)
	// TODO(gabrielwen): Make clusterDomain an option.
	service := fmt.Sprintf("%s.%s.svc.cluster.local", name, namespace)

	vsvc := &unstructured.Unstructured{}
	vsvc.SetAPIVersion("networking.istio.io/v1alpha3")
	vsvc.SetKind("VirtualService")
	vsvc.SetName(virtualServiceName(name, namespace))
	vsvc.SetNamespace(namespace)
	if err := unstructured.SetNestedStringSlice(vsvc.Object, []string{"*"}, "spec", "hosts"); err != nil {
		return nil, fmt.Errorf("Set .spec.hosts error: %v", err)
	}

	istioGateway := os.Getenv("ISTIO_GATEWAY")
	if len(istioGateway) == 0 {
		istioGateway = "kubeflow/kubeflow-gateway"
	}
	if err := unstructured.SetNestedStringSlice(vsvc.Object, []string{istioGateway},
		"spec", "gateways"); err != nil {
		return nil, fmt.Errorf("Set .spec.gateways error: %v", err)
	}

	http := []interface{}{
		map[string]interface{}{
			"match": []interface{}{
				map[string]interface{}{
					"uri": map[string]interface{}{
						"prefix": prefix,
					},
				},
			},
			"rewrite": map[string]interface{}{
				"uri": rewrite,
			},
			"route": []interface{}{
				map[string]interface{}{
					"destination": map[string]interface{}{
						"host": service,
						"port": map[string]interface{}{
							"number": int64(DefaultServingPort),
						},
					},
				},
			},
			"timeout": "300s",
		},
	}
	if err := unstructured.SetNestedSlice(vsvc.Object, http, "spec", "http"); err != nil {
		return nil, fmt.Errorf("Set .spec.http error: %v", err)
	}

	return vsvc, nil

}

func (r *NotebookReconciler) reconcileVirtualService(instance *v1beta1.Notebook) error {
	log := r.Log.WithValues("notebook", instance.Namespace)
	virtualService, err := generateVirtualService(instance)
	if err := ctrl.SetControllerReference(instance, virtualService, r.Scheme); err != nil {
		return err
	}
	// Check if the virtual service already exists.
	foundVirtual := &unstructured.Unstructured{}
	justCreated := false
	foundVirtual.SetAPIVersion("networking.istio.io/v1alpha3")
	foundVirtual.SetKind("VirtualService")
	err = r.Get(context.TODO(), types.NamespacedName{Name: virtualServiceName(instance.Name,
		instance.Namespace), Namespace: instance.Namespace}, foundVirtual)
	if err != nil && apierrs.IsNotFound(err) {
		log.Info("Creating virtual service", "namespace", instance.Namespace, "name",
			virtualServiceName(instance.Name, instance.Namespace))
		err = r.Create(context.TODO(), virtualService)
		justCreated = true
		if err != nil {
			return err
		}
	} else if err != nil {
		return err
	}

	if !justCreated && reconcilehelper.CopyVirtualService(virtualService, foundVirtual) {
		log.Info("Updating virtual service", "namespace", instance.Namespace, "name",
			virtualServiceName(instance.Name, instance.Namespace))
		err = r.Update(context.TODO(), foundVirtual)
		if err != nil {
			return err
		}
	}

	return nil
}

func isStsOrPodEvent(event *corev1.Event) bool {
	return event.InvolvedObject.Kind == "Pod" || event.InvolvedObject.Kind == "StatefulSet"
}

func nbNameFromInvolvedObject(c client.Client, object *corev1.ObjectReference) (string, error) {
	name, namespace := object.Name, object.Namespace

	if object.Kind == "StatefulSet" {
		return name, nil
	}
	if object.Kind == "Pod" {
		pod := &corev1.Pod{}
		err := c.Get(
			context.TODO(),
			types.NamespacedName {
				Namespace: namespace,
				Name:      name,
			},
			pod,
		)
		if err != nil {
			return "", err
		}
		if nbName, ok := pod.Labels["notebook-name"]; ok {
			return nbName, nil
		}
	}
	return "", fmt.Errorf("object isn't related to a Notebook")
}

func nbNameExists(client client.Client, nbName string, namespace string) bool {
	if err := client.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: nbName}, &v1beta1.Notebook{}); err != nil {
		// If error != NotFound, trigger the reconcile call anyway to avoid loosing a potential relevant event
		return !apierrs.IsNotFound(err)
	}
	return true
}

func (r *NotebookReconciler) SetupWithManager(mgr ctrl.Manager) error {
	builder := ctrl.NewControllerManagedBy(mgr).
		For(&v1beta1.Notebook{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{})
	// watch Istio virtual service
	if os.Getenv("USE_ISTIO") == "true" {
		virtualService := &unstructured.Unstructured{}
		virtualService.SetAPIVersion("networking.istio.io/v1alpha3")
		virtualService.SetKind("VirtualService")
		builder.Owns(virtualService)
	}
	builder.WithOptions(controller.Options{MaxConcurrentReconciles: 1})

	// TODO(lunkai): After this is fixed:
	// https://github.com/kubernetes-sigs/controller-runtime/issues/572
	// We don't have to call Build to get the controller.
	c, err := builder.Build(r)
	if err != nil {
		return err
	}

	// We're adding Pods associated with the notebook stateful sets to be enqueued upon
	// Update and Creation, so that they maybe handled during reconciliation
	// watch underlying pod
	mapFn := handler.ToRequestsFunc(
		func(a handler.MapObject) []ctrl.Request {
			return []ctrl.Request{
				{NamespacedName: types.NamespacedName{
					Name:      a.Meta.GetLabels()["notebook-name"],
					Namespace: a.Meta.GetNamespace(),
				}},
			}
		})

	p := predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			// Check if event is a notebook-event
			if _, ok := e.MetaOld.GetLabels()["notebook-name"]; !ok {
				return false
			}
			// Return True if the object updated
			return e.ObjectOld != e.ObjectNew
		},
		CreateFunc: func(e event.CreateEvent) bool {
			// Check if event is a notebook-event
			if _, ok := e.Meta.GetLabels()["notebook-name"]; !ok {
				return false
			}
			// Return true if the notebook-event object was created
			return true
		},
	}

	// Not to be confused with events handled by eventhandlers, these are
	// the k8s Event kind that will be enqueued as reconcile.requests
	// We filter for Event kinds for Creation/Updates on Sts or Pods
	eventToRequest := handler.ToRequestsFunc(
		func(a handler.MapObject) []ctrl.Request {
			return []reconcile.Request{
				{NamespacedName: types.NamespacedName{
					Name:      a.Meta.GetName(),
					Namespace: a.Meta.GetNamespace(),
				}},
			}
		})

	eventsPredicates := predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			event := e.ObjectNew.(*corev1.Event)
			nbName, err := nbNameFromInvolvedObject(r.Client, &event.InvolvedObject)
			if err != nil {
				return false
			}
			return e.ObjectOld != e.ObjectNew &&
				isStsOrPodEvent(event) &&
				nbNameExists(r.Client, nbName, e.MetaNew.GetNamespace())
		},
		CreateFunc: func(e event.CreateEvent) bool {
			event := e.Object.(*corev1.Event)
			nbName, err := nbNameFromInvolvedObject(r.Client, &event.InvolvedObject)
			if err != nil {
				return false
			}
			return isStsOrPodEvent(event) &&
				nbNameExists(r.Client, nbName, e.Meta.GetNamespace())
		},
	}

	// TODO (Humair): Add a watch on Job events with a label: "jobType=ScaleJob"
	// These watches will enqueue Pods and (sts/pod) Events upon Update/Creation.
	if err = c.Watch(
		&source.Kind{Type: &corev1.Pod{}},
		&handler.EnqueueRequestsFromMapFunc{
			ToRequests: mapFn,
		},
		p); err != nil {
		return err
	}

	if err = c.Watch(
		&source.Kind{Type: &corev1.Event{}},
		&handler.EnqueueRequestsFromMapFunc{
			ToRequests: eventToRequest,
		},
		eventsPredicates); err != nil {
		return err
	}

	return nil
}
