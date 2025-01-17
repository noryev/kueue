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

package scheduler

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1alpha2"
	"sigs.k8s.io/kueue/pkg/cache"
	"sigs.k8s.io/kueue/pkg/constants"
	"sigs.k8s.io/kueue/pkg/metrics"
	"sigs.k8s.io/kueue/pkg/queue"
	"sigs.k8s.io/kueue/pkg/scheduler/flavorassigner"
	"sigs.k8s.io/kueue/pkg/util/api"
	"sigs.k8s.io/kueue/pkg/util/routine"
	"sigs.k8s.io/kueue/pkg/workload"
)

const (
	errCouldNotAdmitWL = "Could not admit Workload and assign flavors in apiserver"
)

type Scheduler struct {
	queues                  *queue.Manager
	cache                   *cache.Cache
	client                  client.Client
	recorder                record.EventRecorder
	admissionRoutineWrapper routine.Wrapper

	// Stubs.
	applyAdmission func(context.Context, *kueue.Workload) error
}

func New(queues *queue.Manager, cache *cache.Cache, cl client.Client, recorder record.EventRecorder) *Scheduler {
	s := &Scheduler{
		queues:                  queues,
		cache:                   cache,
		client:                  cl,
		recorder:                recorder,
		admissionRoutineWrapper: routine.DefaultWrapper,
	}
	s.applyAdmission = s.applyAdmissionWithSSA
	return s
}

func (s *Scheduler) Start(ctx context.Context) {
	log := ctrl.LoggerFrom(ctx).WithName("scheduler")
	ctx = ctrl.LoggerInto(ctx, log)
	wait.UntilWithContext(ctx, s.schedule, 0)
}

func (s *Scheduler) setAdmissionRoutineWrapper(wrapper routine.Wrapper) {
	s.admissionRoutineWrapper = wrapper
}

func (s *Scheduler) schedule(ctx context.Context) {
	log := ctrl.LoggerFrom(ctx)

	// 1. Get the heads from the queues, including their desired clusterQueue.
	// This operation blocks while the queues are empty.
	headWorkloads := s.queues.Heads(ctx)
	// No elements means the program is finishing.
	if len(headWorkloads) == 0 {
		return
	}
	startTime := time.Now()

	// 2. Take a snapshot of the cache.
	snapshot := s.cache.Snapshot()

	// 3. Calculate requirements (resource flavors, borrowing) for admitting workloads.
	entries := s.nominate(ctx, headWorkloads, snapshot)

	// 4. Sort entries based on borrowing and timestamps.
	sort.Sort(entryOrdering(entries))

	// 5. Admit entries, ensuring that no more than one workload gets
	// admitted by a cohort (if borrowing).
	// This is because there can be other workloads deeper in a clusterQueue whose
	// head got admitted that should be scheduled in the cohort before the heads
	// of other clusterQueues.
	usedCohorts := sets.NewString()
	for i := range entries {
		e := &entries[i]
		if e.status != nominated {
			continue
		}
		c := snapshot.ClusterQueues[e.ClusterQueue]
		if e.assignment.Borrows() && c.Cohort != nil && usedCohorts.Has(c.Cohort.Name) {
			e.status = skipped
			e.inadmissibleMsg = "cohort used in this cycle"
			continue
		}
		log := log.WithValues("workload", klog.KObj(e.Obj), "clusterQueue", klog.KRef("", e.ClusterQueue))
		if err := s.admit(ctrl.LoggerInto(ctx, log), e); err != nil {
			e.inadmissibleMsg = fmt.Sprintf("Failed to admit workload: %v", err)
		}
		// Even if there was a failure, we shouldn't admit other workloads to this
		// cohort.
		if c.Cohort != nil {
			usedCohorts.Insert(c.Cohort.Name)
		}
	}

	// 6. Requeue the heads that were not scheduled.
	result := metrics.AdmissionResultInadmissible
	for _, e := range entries {
		log.V(3).Info("Workload evaluated for admission",
			"workload", klog.KObj(e.Obj),
			"clusterQueue", klog.KRef("", e.ClusterQueue),
			"status", e.status,
			"reason", e.inadmissibleMsg)
		if e.status != assumed {
			s.requeueAndUpdate(log, ctx, e)
		} else {
			result = metrics.AdmissionResultSuccess
		}
	}
	metrics.AdmissionAttempt(result, time.Since(startTime))
}

type entryStatus string

const (
	// indicates if the workload was nominated for admission.
	nominated entryStatus = "nominated"
	// indicates if the workload was nominated but skipped in this cycle.
	skipped entryStatus = "skipped"
	// indicates if the workload was assumed to have been admitted.
	assumed entryStatus = "assumed"
	// indicates that the workload was never nominated for admission.
	notNominated entryStatus = ""
)

// entry holds requirements for a workload to be admitted by a clusterQueue.
type entry struct {
	// workload.Info holds the workload from the API as well as resource usage
	// and flavors assigned.
	workload.Info
	assignment      flavorassigner.Assignment
	status          entryStatus
	inadmissibleMsg string
	requeueReason   queue.RequeueReason
}

// nominate returns the workloads with their requirements (resource flavors, borrowing) if
// they were admitted by the clusterQueues in the snapshot.
func (s *Scheduler) nominate(ctx context.Context, workloads []workload.Info, snap cache.Snapshot) []entry {
	log := ctrl.LoggerFrom(ctx)
	entries := make([]entry, 0, len(workloads))
	for _, w := range workloads {
		log := log.WithValues("workload", klog.KObj(w.Obj), "clusterQueue", klog.KRef("", w.ClusterQueue))
		cq := snap.ClusterQueues[w.ClusterQueue]
		ns := corev1.Namespace{}
		e := entry{Info: w}
		if snap.InactiveClusterQueueSets.Has(w.ClusterQueue) {
			e.inadmissibleMsg = fmt.Sprintf("ClusterQueue %s is inactive", w.ClusterQueue)
		} else if cq == nil {
			e.inadmissibleMsg = fmt.Sprintf("ClusterQueue %s not found", w.ClusterQueue)
		} else if err := s.client.Get(ctx, types.NamespacedName{Name: w.Obj.Namespace}, &ns); err != nil {
			e.inadmissibleMsg = fmt.Sprintf("Could not obtain workload namespace: %v", err)
		} else if !cq.NamespaceSelector.Matches(labels.Set(ns.Labels)) {
			e.inadmissibleMsg = "Workload namespace doesn't match ClusterQueue selector"
			e.requeueReason = queue.RequeueReasonNamespaceMismatch
		} else if assignment, status := flavorassigner.AssignFlavors(log, &e.Info, snap.ResourceFlavors, cq); !status.IsSuccess() {
			e.inadmissibleMsg = api.TruncateEventMessage(status.Message())
		} else {
			e.assignment = *assignment
			e.status = nominated
		}
		entries = append(entries, e)
	}
	return entries
}

// admit sets the admitting clusterQueue and flavors into the workload of
// the entry, and asynchronously updates the object in the apiserver after
// assuming it in the cache.
func (s *Scheduler) admit(ctx context.Context, e *entry) error {
	log := ctrl.LoggerFrom(ctx)
	newWorkload := e.Obj.DeepCopy()
	admission := &kueue.Admission{
		ClusterQueue:  kueue.ClusterQueueReference(e.ClusterQueue),
		PodSetFlavors: e.assignment.ToAPI(),
	}
	newWorkload.Spec.Admission = admission
	if err := s.cache.AssumeWorkload(newWorkload); err != nil {
		return err
	}
	e.status = assumed
	log.V(2).Info("Workload assumed in the cache")

	s.admissionRoutineWrapper.Run(func() {
		err := s.applyAdmission(ctx, workloadAdmissionFrom(newWorkload))
		if err == nil {
			waitTime := time.Since(e.Obj.CreationTimestamp.Time)
			s.recorder.Eventf(newWorkload, corev1.EventTypeNormal, "Admitted", "Admitted by ClusterQueue %v, wait time was %.3fs", admission.ClusterQueue, waitTime.Seconds())
			metrics.AdmittedWorkload(admission.ClusterQueue, waitTime)
			log.V(2).Info("Workload successfully admitted and assigned flavors")
			return
		}
		// Ignore errors because the workload or clusterQueue could have been deleted
		// by an event.
		_ = s.cache.ForgetWorkload(newWorkload)
		if errors.IsNotFound(err) {
			log.V(2).Info("Workload not admitted because it was deleted")
			return
		}

		log.Error(err, errCouldNotAdmitWL)
		s.requeueAndUpdate(log, ctx, *e)
	})

	return nil
}

func (s *Scheduler) applyAdmissionWithSSA(ctx context.Context, w *kueue.Workload) error {
	return s.client.Patch(ctx, w, client.Apply, client.FieldOwner(constants.AdmissionName))
}

// workloadAdmissionFrom returns only the fields necessary for admission using
// ServerSideApply.
func workloadAdmissionFrom(w *kueue.Workload) *kueue.Workload {
	wlCopy := &kueue.Workload{
		ObjectMeta: metav1.ObjectMeta{
			UID:        w.UID,
			Name:       w.Name,
			Namespace:  w.Namespace,
			Generation: w.Generation, // Produce a conflict if there was a change in the spec.
		},
		TypeMeta: w.TypeMeta,
		Spec: kueue.WorkloadSpec{
			Admission: w.Spec.Admission.DeepCopy(),
		},
	}
	if wlCopy.APIVersion == "" {
		wlCopy.APIVersion = kueue.GroupVersion.String()
	}
	if wlCopy.Kind == "" {
		wlCopy.Kind = "Workload"
	}
	return wlCopy
}

type entryOrdering []entry

func (e entryOrdering) Len() int {
	return len(e)
}

func (e entryOrdering) Swap(i, j int) {
	e[i], e[j] = e[j], e[i]
}

// Less is the ordering criteria:
// 1. request under min quota before borrowing.
// 2. FIFO on creation timestamp.
func (e entryOrdering) Less(i, j int) bool {
	a := e[i]
	b := e[j]
	// 1. Request under min quota.
	aBorrows := a.assignment.Borrows()
	bBorrows := b.assignment.Borrows()
	if aBorrows != bBorrows {
		return !aBorrows
	}
	// 2. FIFO.
	return a.Obj.CreationTimestamp.Before(&b.Obj.CreationTimestamp)
}

func (s *Scheduler) requeueAndUpdate(log logr.Logger, ctx context.Context, e entry) {
	if e.status != notNominated && e.requeueReason == queue.RequeueReasonGeneric {
		// Failed after nomination is the only reason why a workload would be requeued downstream.
		e.requeueReason = queue.RequeueReasonFailedAfterNomination
	}
	added := s.queues.RequeueWorkload(ctx, &e.Info, e.requeueReason)
	log.V(2).Info("Workload re-queued", "workload", klog.KObj(e.Obj), "clusterQueue", e.ClusterQueue, "queue", klog.KRef(e.Obj.Namespace, e.Obj.Spec.QueueName), "requeueReason", e.requeueReason, "added", added)

	if e.status == notNominated {
		err := workload.UpdateStatus(ctx, s.client, e.Obj, kueue.WorkloadAdmitted, metav1.ConditionFalse, "Pending", e.inadmissibleMsg)
		if err != nil {
			log.Error(err, "Could not update Workload status")
		}
		s.recorder.Eventf(e.Obj, corev1.EventTypeNormal, "Pending", e.inadmissibleMsg)
	}
}
