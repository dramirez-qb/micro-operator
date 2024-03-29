package micro

import (
	"context"
	"reflect"
	"sync"

	microv1alpha1 "github.com/micro/micro-operator/pkg/apis/micro/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// configure operator logger
var log = logf.Log.WithName("micro")

// blank assignment to verify that ReconcileMicro implements reconcile.Reconciler
var _ reconcile.Reconciler = &ReconcileMicro{}

// Add creates a new Micro Controller and adds it to the Manager.
// The Manager will set fields on the Controller and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	return add(mgr, newReconciler(mgr))
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new Micro controller
	c, err := controller.New("micro-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary Micro resource
	err = c.Watch(&source.Kind{Type: &microv1alpha1.Micro{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	// Watch for changes to secondary Micro resources
	err = c.Watch(&source.Kind{Type: &appsv1.Deployment{}}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &microv1alpha1.Micro{},
	})
	if err != nil {
		return err
	}

	return nil
}

// ReconcileMicro reconciles a Micro object
type ReconcileMicro struct {
	// This client, initialized using mgr.Client(), is a split client
	// that reads objects from the cache and writes to the apiserver
	client client.Client
	scheme *runtime.Scheme
	// micro deployments
	// NOTE: Operator is single threaded so probably doesnt need mutex
	sync.RWMutex
	micro map[string]*appsv1.Deployment
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) reconcile.Reconciler {
	r := &ReconcileMicro{
		client: mgr.GetClient(),
		scheme: mgr.GetScheme(),
		micro:  make(map[string]*appsv1.Deployment),
	}

	// TODO: launch a goroutine that
	// - monitors some remote URL
	// - if it detects new hub build it triggers new deployment

	return r
}

// Reconcile reads that state of the cluster for a Micro object and makes changes based on the state read
// and what is in the Micro.Spec
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileMicro) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	reqLogger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.Info("Reconciling Micro")

	// kind of micro CRD: micro-api, etc.
	kind := request.Name

	// Fetch the Micro CRD instance
	micro := &microv1alpha1.Micro{}
	err := r.client.Get(context.TODO(), request.NamespacedName, micro)
	if err != nil {
		if errors.IsNotFound(err) {
			// Custom Resource object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			reqLogger.Info("No Micro resource found. Ignoring since CRD object must have been deleted")
			// if it no longer exists, remove it from the platform deployments
			r.Lock()
			delete(r.micro, kind)
			r.Unlock()
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		reqLogger.Error(err, "Failed to get Micro", "Micro.Kind", kind)
		return reconcile.Result{}, err
	}

	// Check if particular Micro Deployment already exists
	found := &appsv1.Deployment{}
	err = r.client.Get(context.TODO(), types.NamespacedName{Name: micro.Name, Namespace: micro.Namespace}, found)
	if err != nil && errors.IsNotFound(err) {
		// check if we have the deployment for this dep stored, if yes make sure we create it
		r.RLock()
		// check if we are tracking the deployment already
		// if yes attempt to reoncile non-existent micro resource
		if dep, ok := r.micro[kind]; ok {
			reqLogger.Info("Micro Deployment", "Micro.Kind", micro.Kind, "Micro.Namespace", micro.Namespace)
			// create platform deployment
			err = r.client.Create(context.TODO(), dep)
			if err != nil {
				r.RUnlock()
				return reconcile.Result{}, err
			}
		}
		r.RUnlock()
		// Deployment created successfully - don't requeue
		return reconcile.Result{Requeue: true}, nil
	} else if err != nil {
		reqLogger.Error(err, "Failed to get Deployment")
		return reconcile.Result{}, err
	}

	// store the deployment and make it the owner of its resources
	r.Lock()
	if _, ok := r.micro[kind]; !ok {
		r.micro[kind] = found
		// Set Micro micro as the owner and controller
		if err := controllerutil.SetControllerReference(micro, found, r.scheme); err != nil {
			r.Unlock()
			return reconcile.Result{}, err
		}
	}
	r.Unlock()

	// Ensure the deployment size is the same as the spec
	size := micro.Spec.Size
	if *found.Spec.Replicas != size {
		found.Spec.Replicas = &size
		err = r.client.Update(context.TODO(), found)
		if err != nil {
			reqLogger.Error(err, "Failed to update Micro", "Micro.Kind", micro.Kind, "Micro.Namespace", micro.Namespace)
			return reconcile.Result{}, err
		}
		// Spec updated - return and requeue
		return reconcile.Result{Requeue: true}, nil
	}

	// Update the Micro status with the pod names
	// List the pods for this micro deployment
	labels := map[string]string{
		"name": kind,
	}
	podList := &corev1.PodList{}
	listOpts := []client.ListOption{
		client.InNamespace(micro.Namespace),
		client.MatchingLabels(labels),
	}
	if err = r.client.List(context.TODO(), podList, listOpts...); err != nil {
		reqLogger.Error(err, "Failed to list Micro pods", "Micro.Kind", micro.Kind, "Micro.Namespace", micro.Namespace)
		return reconcile.Result{}, err
	}

	// get a list of returned pods for the micro deployment
	podNames := make([]string, len(podList.Items))
	for i, pod := range podList.Items {
		podNames[i] = pod.Name
	}

	// Update status.Nodes if needed
	if !reflect.DeepEqual(podNames, micro.Status.Nodes) {
		micro.Status.Nodes = podNames
		err := r.client.Status().Update(context.TODO(), micro)
		if err != nil {
			reqLogger.Error(err, "Failed to update Micro status")
			return reconcile.Result{}, err
		}
	}

	// Pod already exists - don't requeue
	reqLogger.Info("Skip reconcile: Micro Deployment already exists", "Micro.Kind", micro.Kind, "Micro.Namespace", micro.Namespace)
	return reconcile.Result{}, nil
}
