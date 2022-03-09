/*
Copyright 2022 The Kubernetes Authors.

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

package job

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"k8s.io/utils/pointer"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kueue "sigs.k8s.io/kueue/api/v1alpha1"
	"sigs.k8s.io/kueue/pkg/constants"
	"sigs.k8s.io/kueue/pkg/workload"
)

var (
	ownerKey = ".metadata.controller"
)

// JobReconciler reconciles a Job object
type JobReconciler struct {
	client client.Client
	scheme *runtime.Scheme
	record record.EventRecorder
}

func NewReconciler(scheme *runtime.Scheme, client client.Client, record record.EventRecorder) *JobReconciler {
	return &JobReconciler{
		scheme: scheme,
		client: client,
		record: record,
	}
}

// SetupWithManager sets up the controller with the Manager. It indexes workloads
// based on the owning jobs.
func (r *JobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &kueue.QueuedWorkload{}, ownerKey, func(rawObj client.Object) []string {
		// grab the QueuedWorkload object, extract the owner...
		qw := rawObj.(*kueue.QueuedWorkload)
		owner := metav1.GetControllerOf(qw)
		if owner == nil {
			return nil
		}
		// ...make sure it's a Job...
		if owner.APIVersion != "batch/v1" || owner.Kind != "Job" {
			return nil
		}

		// ...and if so, return it
		return []string{owner.Name}
	}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&batchv1.Job{}).
		Owns(&kueue.QueuedWorkload{}).
		Complete(r)
}

//+kubebuilder:rbac:groups="",resources=events,verbs=create;watch;update
//+kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=batch,resources=jobs/status,verbs=get
//+kubebuilder:rbac:groups=kueue.x-k8s.io,resources=queuedworkloads,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=kueue.x-k8s.io,resources=queuedworkloads/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=kueue.x-k8s.io,resources=queuedworkloads/finalizers,verbs=update
//+kubebuilder:rbac:groups=kueue.x-k8s.io,resources=clusterqueues,verbs=get;list;watch

func (r *JobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var job batchv1.Job
	if err := r.client.Get(ctx, req.NamespacedName, &job); err != nil {
		// we'll ignore not-found errors, since there is nothing to do.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log := ctrl.LoggerFrom(ctx).WithValues("job", klog.KObj(&job))
	ctx = ctrl.LoggerInto(ctx, log)
	log.V(2).Info("Reconciling Job")

	var childWorkloads kueue.QueuedWorkloadList
	if err := r.client.List(ctx, &childWorkloads, client.InNamespace(req.Namespace),
		client.MatchingFields{ownerKey: req.Name}); err != nil {
		log.Error(err, "Unable to list child workloads")
		return ctrl.Result{}, err
	}

	// 1. make sure there is only a single existing instance of the workload
	wl, err := r.ensureAtMostOneWorkload(ctx, &job, childWorkloads)
	if err != nil {
		log.Error(err, "Getting existing workloads")
		return ctrl.Result{}, err
	}

	jobFinishedCond, jobFinished := jobFinishedCondition(&job)
	// 2. create new workload if none exists
	if wl == nil {
		// Nothing to do if the job is finished
		if jobFinished {
			return ctrl.Result{}, nil
		}
		err := r.handleJobWithNoWorkload(ctx, &job)
		if err != nil {
			log.Error(err, "Handling job with no workload")
		}
		return ctrl.Result{}, err
	}

	// 3. handle a finished job
	if jobFinished {
		added := false
		wl.Status.Conditions, added = appendFinishedConditionIfNotExists(wl.Status.Conditions, jobFinishedCond)
		if !added {
			return ctrl.Result{}, nil
		}
		err := r.client.Status().Update(ctx, wl)
		if err != nil {
			log.Error(err, "Updating workload status")
		}
		return ctrl.Result{}, err
	}

	// 4. Handle a not finished job
	if jobSuspended(&job) {
		// 4.1 start the job if the workload has been admitted, and the job is still suspended
		if wl.Spec.Admission != nil {
			log.V(2).Info("Job admitted, unsuspending")
			err := r.startJob(ctx, wl, &job)
			if err != nil {
				log.Error(err, "Unsuspending job")
			}
			return ctrl.Result{}, err
		}

		// 4.2 update queue name if changed.
		q := queueName(&job)
		if wl.Spec.QueueName != q {
			log.V(2).Info("Job changed queues, updating workload")
			wl.Spec.QueueName = q
			err := r.client.Update(ctx, wl)
			if err != nil {
				log.Error(err, "Updating workload queue")
			}
			return ctrl.Result{}, err
		}
		log.V(3).Info("Job is suspended and workload not yet admitted by a clusterQueue, nothing to do")
		return ctrl.Result{}, nil
	}

	if wl.Spec.Admission == nil {
		// 4.3 the job must be suspended if the workload is not yet admitted.
		log.V(2).Info("Running job is not admitted by a cluster queue, suspending")
		err := r.stopJob(ctx, wl, &job, "Not admitted by cluster queue")
		if err != nil {
			log.Error(err, "Suspending job with non admitted workload")
		}
		return ctrl.Result{}, err
	}

	// 4.4 workload is admitted and job is running, nothing to do.
	log.V(3).Info("Job running with admitted workload, nothing to do")
	return ctrl.Result{}, nil

}

// stopJob sends updates to suspend the job, reset the startTime so we can update the scheduling directives
// later when unsuspending and resets the nodeSelector to its previous state based on what is available in
// the workload (which should include the original affinities that the job had).
func (r *JobReconciler) stopJob(ctx context.Context, w *kueue.QueuedWorkload,
	job *batchv1.Job, eventMsg string) error {
	job.Spec.Suspend = pointer.BoolPtr(true)
	if err := r.client.Update(ctx, job); err != nil {
		return err
	}
	r.record.Eventf(job, corev1.EventTypeNormal, "Stopped", eventMsg)

	// Reset start time so we can update the scheduling directives later when unsuspending.
	if job.Status.StartTime != nil {
		job.Status.StartTime = nil
		if err := r.client.Status().Update(ctx, job); err != nil {
			return err
		}
	}

	if w != nil && !equality.Semantic.DeepEqual(job.Spec.Template.Spec.NodeSelector,
		w.Spec.PodSets[0].Spec.NodeSelector) {
		job.Spec.Template.Spec.NodeSelector = map[string]string{}
		for k, v := range w.Spec.PodSets[0].Spec.NodeSelector {
			job.Spec.Template.Spec.NodeSelector[k] = v
		}
		return r.client.Update(ctx, job)
	}

	return nil
}

func (r *JobReconciler) startJob(ctx context.Context, w *kueue.QueuedWorkload, job *batchv1.Job) error {
	log := ctrl.LoggerFrom(ctx)

	// Lookup the clusterQueue to fetch the node affinity labels to apply on the job.
	clusterQueue := kueue.ClusterQueue{}
	if err := r.client.Get(ctx, types.NamespacedName{Name: string(w.Spec.Admission.ClusterQueue)}, &clusterQueue); err != nil {
		log.Error(err, "fetching ClusterQueue")
		return err
	}

	if len(w.Spec.PodSets) != 1 {
		return fmt.Errorf("one podset must exist, found %d", len(w.Spec.PodSets))
	}
	nodeSelector := getNodeSelectors(&clusterQueue, w)
	if len(nodeSelector) != 0 {
		if job.Spec.Template.Spec.NodeSelector == nil {
			job.Spec.Template.Spec.NodeSelector = nodeSelector
		} else {
			for k, v := range nodeSelector {
				job.Spec.Template.Spec.NodeSelector[k] = v
			}
		}

	} else {
		log.V(3).Info("no nodeSelectors to inject")
	}

	job.Spec.Suspend = pointer.BoolPtr(false)
	if err := r.client.Update(ctx, job); err != nil {
		return err
	}

	r.record.Eventf(job, corev1.EventTypeNormal, "Started",
		"Admitted by clusterQueue %v", w.Spec.Admission.ClusterQueue)
	return nil
}

func getNodeSelectors(cq *kueue.ClusterQueue, w *kueue.QueuedWorkload) map[string]string {
	if len(w.Spec.Admission.PodSetFlavors[0].ResourceFlavors) == 0 {
		return nil
	}

	// Create a map of resources-to-flavors.
	// May be cache this?
	flavors := make(map[corev1.ResourceName]map[string]*kueue.ResourceFlavor, len(cq.Spec.RequestableResources))
	for _, r := range cq.Spec.RequestableResources {
		flavors[r.Name] = make(map[string]*kueue.ResourceFlavor, len(r.Flavors))
		for j := range r.Flavors {
			flavors[r.Name][r.Flavors[j].Name] = &r.Flavors[j]
		}
	}

	nodeSelector := map[string]string{}
	for res, flvr := range w.Spec.Admission.PodSetFlavors[0].ResourceFlavors {
		if cqRes, existRes := flavors[res]; existRes {
			if cqFlvr, existFlvr := cqRes[flvr]; existFlvr {
				for k, v := range cqFlvr.Labels {
					nodeSelector[k] = v
				}
			}
		}
	}
	return nodeSelector
}

func (r *JobReconciler) handleJobWithNoWorkload(ctx context.Context, job *batchv1.Job) error {
	log := ctrl.LoggerFrom(ctx)

	// Wait until there are no active pods.
	if job.Status.Active != 0 {
		log.V(2).Info("Job is suspended but still has active pods, waiting")
		return nil
	}

	// Create the corresponding workload.
	wl, err := ConstructWorkloadFor(job, r.scheme)
	if err != nil {
		return err
	}
	if err = r.client.Create(ctx, wl); err != nil {
		return err
	}

	r.record.Eventf(job, corev1.EventTypeNormal, "CreatedQueuedWorkload",
		"Created QueuedWorkload: %v", workload.Key(wl))
	return nil
}

// ensureAtmostoneworkload finds a matching workload and deletes redundant ones.
func (r *JobReconciler) ensureAtMostOneWorkload(ctx context.Context, job *batchv1.Job, workloads kueue.QueuedWorkloadList) (*kueue.QueuedWorkload, error) {
	log := ctrl.LoggerFrom(ctx)

	// Find a matching workload first if there is one.
	var toDelete []*kueue.QueuedWorkload
	var match *kueue.QueuedWorkload
	for i := range workloads.Items {
		w := &workloads.Items[i]
		owner := metav1.GetControllerOf(w)
		// Indexes don't work in unit tests, so we explicitly check for the
		// owner here.
		if owner.Name != job.Name {
			continue
		}
		if match == nil && jobAndWorkloadEqual(job, w) {
			match = w
		} else {
			toDelete = append(toDelete, w)
		}
	}

	// If there is no matching workload and the job is running, suspend it.
	if match == nil && !jobSuspended(job) {
		log.V(2).Info("job with no matching workload, suspending")
		var w *kueue.QueuedWorkload
		if len(workloads.Items) == 1 {
			// The job may have been modified and hence the existing workload
			// doesn't match the job anymore. All bets are off if there are more
			// than one workload...
			w = &workloads.Items[0]
		}
		if err := r.stopJob(ctx, w, job, "No matching QueuedWorkload"); err != nil {
			log.Error(err, "stopping job")
		}
	}

	// Delete duplicate workload instances.
	existedWls := 0
	for i := range toDelete {
		err := r.client.Delete(ctx, toDelete[i])
		if err == nil || !apierrors.IsNotFound(err) {
			existedWls++
		}
		if err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "Failed to delete workload")
		}
		if err == nil {
			r.record.Eventf(job, corev1.EventTypeNormal, "DeletedQueuedWorkload",
				"Deleted not matching QueuedWorkload: %v", workload.Key(toDelete[i]))
		}
	}

	if existedWls != 0 {
		if match == nil {
			return nil, fmt.Errorf("no matching workload was found, tried deleting %d existing workload(s)", existedWls)
		}
		return nil, fmt.Errorf("only one workload should exist, found %d", len(workloads.Items))
	}

	return match, nil
}

func ConstructWorkloadFor(job *batchv1.Job, scheme *runtime.Scheme) (*kueue.QueuedWorkload, error) {
	w := &kueue.QueuedWorkload{
		ObjectMeta: metav1.ObjectMeta{
			Name:      job.Name,
			Namespace: job.Namespace,
		},
		Spec: kueue.QueuedWorkloadSpec{
			PodSets: []kueue.PodSet{
				{
					Spec:  *job.Spec.Template.Spec.DeepCopy(),
					Count: *job.Spec.Parallelism,
				},
			},
			QueueName: queueName(job),
		},
	}
	if err := ctrl.SetControllerReference(job, w, scheme); err != nil {
		return nil, err
	}

	return w, nil
}

func appendFinishedConditionIfNotExists(conds []kueue.QueuedWorkloadCondition, jobStatus batchv1.JobConditionType) ([]kueue.QueuedWorkloadCondition, bool) {
	for i, c := range conds {
		if c.Type == kueue.QueuedWorkloadFinished {
			if c.Status == corev1.ConditionTrue {
				return conds, false
			}
			conds = append(conds[:i], conds[i+1:]...)
			break
		}
	}
	message := "Job finished successfully"
	if jobStatus == batchv1.JobFailed {
		message = "Job failed"
	}
	now := metav1.Now()
	conds = append(conds, kueue.QueuedWorkloadCondition{
		Type:               kueue.QueuedWorkloadFinished,
		Status:             corev1.ConditionTrue,
		LastProbeTime:      now,
		LastTransitionTime: now,
		Reason:             "JobFinished",
		Message:            message,
	})
	return conds, true
}

// From https://github.com/kubernetes/kubernetes/blob/master/pkg/controller/job/utils.go
func jobFinishedCondition(j *batchv1.Job) (batchv1.JobConditionType, bool) {
	for _, c := range j.Status.Conditions {
		if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == corev1.ConditionTrue {
			return c.Type, true
		}
	}
	return "", false
}

func jobSuspended(j *batchv1.Job) bool {
	return j.Spec.Suspend != nil && *j.Spec.Suspend

}

func jobAndWorkloadEqual(job *batchv1.Job, wl *kueue.QueuedWorkload) bool {
	if len(wl.Spec.PodSets) != 1 {
		return false
	}
	if *job.Spec.Parallelism != wl.Spec.PodSets[0].Count {
		return false
	}

	// nodeSelector may change, hence we are not checking checking for
	// equality of the whole job.Spec.Template.Spec.
	if !equality.Semantic.DeepEqual(job.Spec.Template.Spec.InitContainers,
		wl.Spec.PodSets[0].Spec.InitContainers) {
		return false
	}
	return equality.Semantic.DeepEqual(job.Spec.Template.Spec.Containers,
		wl.Spec.PodSets[0].Spec.Containers)
}

func queueName(job *batchv1.Job) string {
	return job.Annotations[constants.QueueAnnotation]
}