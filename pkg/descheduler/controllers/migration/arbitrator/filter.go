/*
Copyright 2023 The Koordinator Authors.

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

package arbitrator

import (
	"context"
	"fmt"
	"sync"
	"time"

	gocache "github.com/patrickmn/go-cache"
	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	k8spodutil "k8s.io/kubernetes/pkg/api/v1/pod"
	kubecontroller "k8s.io/kubernetes/pkg/controller"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	k8sdeschedulerapi "sigs.k8s.io/descheduler/pkg/api"

	sev1alpha1 "github.com/koordinator-sh/koordinator/apis/scheduling/v1alpha1"
	deschedulerconfig "github.com/koordinator-sh/koordinator/pkg/descheduler/apis/config"
	"github.com/koordinator-sh/koordinator/pkg/descheduler/controllers/migration/controllerfinder"
	"github.com/koordinator-sh/koordinator/pkg/descheduler/controllers/migration/util"
	"github.com/koordinator-sh/koordinator/pkg/descheduler/controllers/options"
	evictionsutil "github.com/koordinator-sh/koordinator/pkg/descheduler/evictions"
	"github.com/koordinator-sh/koordinator/pkg/descheduler/fieldindex"
	"github.com/koordinator-sh/koordinator/pkg/descheduler/framework"
	"github.com/koordinator-sh/koordinator/pkg/descheduler/framework/plugins/kubernetes/defaultevictor"
	podutil "github.com/koordinator-sh/koordinator/pkg/descheduler/pod"
	pkgutil "github.com/koordinator-sh/koordinator/pkg/util"
	utilclient "github.com/koordinator-sh/koordinator/pkg/util/client"
)

type MigrationFilter interface {
	Filter(pod *corev1.Pod) bool
	PreEvictionFilter(pod *corev1.Pod) bool
	TrackEvictedPod(pod *corev1.Pod)
}

type ArbitrationFilter interface {
	NonRetryablePodFilter(*corev1.Pod) bool
	RetryablePodFilter(*corev1.Pod) bool
}

type filter struct {
	client client.Client
	mu     sync.Mutex
	clock  clock.RealClock

	nonRetryablePodFilter framework.FilterFunc
	retryablePodFilter    framework.FilterFunc
	defaultFilterPlugin   framework.FilterPlugin

	args             *deschedulerconfig.MigrationControllerArgs
	objectLimiters   map[types.UID]*rate.Limiter
	limiterCache     *gocache.Cache
	controllerFinder controllerfinder.Interface
}

func NewFilter(args *deschedulerconfig.MigrationControllerArgs, handle framework.Handle) (MigrationFilter, ArbitrationFilter, error) {
	controllerFinder, err := controllerfinder.New(options.Manager)
	if err != nil {
		return nil, nil, err
	}
	f := &filter{
		client:           options.Manager.GetClient(),
		args:             args,
		controllerFinder: controllerFinder,
		clock:            clock.RealClock{},
	}
	if err := f.initFilters(args, handle); err != nil {
		return nil, nil, err
	}
	f.initObjectLimiters()
	return f, f, nil
}

func (f *filter) initFilters(args *deschedulerconfig.MigrationControllerArgs, handle framework.Handle) error {
	defaultEvictorArgs := &defaultevictor.DefaultEvictorArgs{
		NodeFit:                 args.NodeFit,
		NodeSelector:            args.NodeSelector,
		EvictLocalStoragePods:   args.EvictLocalStoragePods,
		EvictSystemCriticalPods: args.EvictSystemCriticalPods,
		IgnorePvcPods:           args.IgnorePvcPods,
		EvictFailedBarePods:     args.EvictFailedBarePods,
		LabelSelector:           args.LabelSelector,
	}
	if args.PriorityThreshold != nil {
		defaultEvictorArgs.PriorityThreshold = &k8sdeschedulerapi.PriorityThreshold{
			Name:  args.PriorityThreshold.Name,
			Value: args.PriorityThreshold.Value,
		}
	}
	defaultEvictor, err := defaultevictor.New(defaultEvictorArgs, handle)
	if err != nil {
		return err
	}

	var includedNamespaces, excludedNamespaces sets.String
	if args.Namespaces != nil {
		includedNamespaces = sets.NewString(args.Namespaces.Include...)
		excludedNamespaces = sets.NewString(args.Namespaces.Exclude...)
	}

	filterPlugin := defaultEvictor.(framework.FilterPlugin)
	wrapFilterFuncs := podutil.WrapFilterFuncs(
		util.FilterPodWithMaxEvictionCost,
		filterPlugin.Filter,
		f.filterExpectedReplicas,
		f.reservationFilter,
	)
	podFilter, err := podutil.NewOptions().
		WithFilter(wrapFilterFuncs).
		WithNamespaces(includedNamespaces).
		WithoutNamespaces(excludedNamespaces).
		BuildFilterFunc()
	if err != nil {
		return err
	}
	retriablePodFilters := podutil.WrapFilterFuncs(
		f.filterLimitedObject,
		f.filterMaxMigratingPerNode,
		f.filterMaxMigratingPerNamespace,
		f.filterMaxMigratingOrUnavailablePerWorkload,
	)
	f.retryablePodFilter = func(pod *corev1.Pod) bool {
		return evictionsutil.HaveEvictAnnotation(pod) || retriablePodFilters(pod)
	}
	f.nonRetryablePodFilter = podFilter
	f.defaultFilterPlugin = defaultEvictor.(framework.FilterPlugin)
	return nil
}

// Filter checks if a pod can be evicted
func (f *filter) Filter(pod *corev1.Pod) bool {
	if !f.filterExistingPodMigrationJob(pod) {
		return false
	}

	if !f.reservationFilter(pod) {
		return false
	}

	if f.nonRetryablePodFilter != nil && !f.nonRetryablePodFilter(pod) {
		return false
	}
	if f.retryablePodFilter != nil && !f.retryablePodFilter(pod) {
		return false
	}
	return true
}

func (f *filter) NonRetryablePodFilter(pod *corev1.Pod) bool {
	return f.nonRetryablePodFilter(pod)
}

func (f *filter) RetryablePodFilter(pod *corev1.Pod) bool {
	return f.retryablePodFilter(pod)
}

func (f *filter) reservationFilter(pod *corev1.Pod) bool {
	if sev1alpha1.PodMigrationJobMode(f.args.DefaultJobMode) != sev1alpha1.PodMigrationJobModeReservationFirst {
		return true
	}

	if pkgutil.IsIn(f.args.SchedulerNames, pod.Spec.SchedulerName) {
		return true
	}

	klog.Errorf("Pod %q can not be migrated by ReservationFirst mode because pod.schedulerName=%s but scheduler of pmj controller assigned is %s", klog.KObj(pod), pod.Spec.SchedulerName, f.args.SchedulerNames)
	return false
}

func (f *filter) PreEvictionFilter(pod *corev1.Pod) bool {
	return f.defaultFilterPlugin.PreEvictionFilter(pod)
}

func (f *filter) forEachAvailableMigrationJobs(listOpts *client.ListOptions, handler func(job *sev1alpha1.PodMigrationJob) bool, expectedPhaseAndAnnotations ...PhaseAndAnnotation) {
	jobList := &sev1alpha1.PodMigrationJobList{}
	err := f.client.List(context.TODO(), jobList, listOpts, utilclient.DisableDeepCopy)
	if err != nil {
		klog.Errorf("failed to get PodMigrationJobList, err: %v", err)
		return
	}

	if len(expectedPhaseAndAnnotations) == 0 {
		expectedPhaseAndAnnotations = []PhaseAndAnnotation{
			{sev1alpha1.PodMigrationJobRunning, nil},
			{sev1alpha1.PodMigrationJobPending, nil},
		}
	}

	for i := range jobList.Items {
		job := &jobList.Items[i]
		phase := job.Status.Phase
		if phase == "" {
			phase = sev1alpha1.PodMigrationJobPending
		}
		found := false
		for _, v := range expectedPhaseAndAnnotations {
			if v.phase == phase && isContain(job.Annotations, v.annotations) {
				found = true
				break
			}
		}
		if found && !handler(job) {
			break
		}
	}
}

func (f *filter) filterExistingPodMigrationJob(pod *corev1.Pod) bool {
	return !f.existingPodMigrationJob(pod)
}

func (f *filter) existingPodMigrationJob(pod *corev1.Pod, expectedPhaseAndAnnotations ...PhaseAndAnnotation) bool {
	opts := &client.ListOptions{FieldSelector: fields.OneTermEqualSelector(fieldindex.IndexJobByPodUID, string(pod.UID))}
	existing := false
	f.forEachAvailableMigrationJobs(opts, func(job *sev1alpha1.PodMigrationJob) bool {
		if podRef := job.Spec.PodRef; podRef != nil && podRef.UID == pod.UID {
			existing = true
		}
		return !existing
	}, expectedPhaseAndAnnotations...)

	if !existing {
		opts = &client.ListOptions{FieldSelector: fields.OneTermEqualSelector(fieldindex.IndexJobPodNamespacedName, fmt.Sprintf("%s/%s", pod.Namespace, pod.Name))}
		f.forEachAvailableMigrationJobs(opts, func(job *sev1alpha1.PodMigrationJob) bool {
			if podRef := job.Spec.PodRef; podRef != nil && podRef.Namespace == pod.Namespace && podRef.Name == pod.Name {
				existing = true
			}
			return !existing
		}, expectedPhaseAndAnnotations...)
	}
	return existing
}

func (f *filter) filterMaxMigratingPerNode(pod *corev1.Pod) bool {
	if pod.Spec.NodeName == "" || f.args.MaxMigratingPerNode == nil || *f.args.MaxMigratingPerNode <= 0 {
		return true
	}

	podList := &corev1.PodList{}
	listOpts := &client.ListOptions{FieldSelector: fields.OneTermEqualSelector(fieldindex.IndexPodByNodeName, pod.Spec.NodeName)}
	err := f.client.List(context.TODO(), podList, listOpts, utilclient.DisableDeepCopy)
	if err != nil {
		return true
	}
	if len(podList.Items) == 0 {
		return true
	}

	var expectedPhaseAndAnnotations []PhaseAndAnnotation
	expectedPhaseAndAnnotations = []PhaseAndAnnotation{
		{sev1alpha1.PodMigrationJobRunning, nil},
		{sev1alpha1.PodMigrationJobPending, map[string]string{AnnotationPassedArbitration: "true"}},
	}

	count := 0
	for i := range podList.Items {
		v := &podList.Items[i]
		if v.UID != pod.UID &&
			v.Spec.NodeName == pod.Spec.NodeName &&
			f.existingPodMigrationJob(v, expectedPhaseAndAnnotations...) {
			count++
		}
	}

	maxMigratingPerNode := int(*f.args.MaxMigratingPerNode)
	exceeded := count >= maxMigratingPerNode
	if exceeded {
		klog.V(4).Infof("Pod %q fails to check maxMigratingPerNode because the Node %q has %d migrating Pods, exceeding the maxMigratingPerNode(%d)",
			klog.KObj(pod), pod.Spec.NodeName, count, maxMigratingPerNode)
	}
	return !exceeded
}

func (f *filter) filterMaxMigratingPerNamespace(pod *corev1.Pod) bool {
	if f.args.MaxMigratingPerNamespace == nil || *f.args.MaxMigratingPerNamespace <= 0 {
		return true
	}

	var expectedPhaseAndAnnotations []PhaseAndAnnotation
	expectedPhaseAndAnnotations = []PhaseAndAnnotation{
		{sev1alpha1.PodMigrationJobRunning, nil},
		{sev1alpha1.PodMigrationJobPending, map[string]string{AnnotationPassedArbitration: "true"}},
	}

	opts := &client.ListOptions{FieldSelector: fields.OneTermEqualSelector(fieldindex.IndexJobByPodNamespace, pod.Namespace)}
	count := 0
	f.forEachAvailableMigrationJobs(opts, func(job *sev1alpha1.PodMigrationJob) bool {
		if podRef := job.Spec.PodRef; podRef != nil && podRef.UID != pod.UID && podRef.Namespace == pod.Namespace {
			count++
		}
		return true
	}, expectedPhaseAndAnnotations...)

	maxMigratingPerNamespace := int(*f.args.MaxMigratingPerNamespace)
	exceeded := count >= maxMigratingPerNamespace
	if exceeded {
		klog.V(4).Infof("Pod %q fails to check maxMigratingPerNamespace because the Namespace %q has %d migrating Pods, exceeding the maxMigratingPerNamespace(%d)",
			klog.KObj(pod), pod.Namespace, count, maxMigratingPerNamespace)
	}
	return !exceeded
}

func (f *filter) filterMaxMigratingOrUnavailablePerWorkload(pod *corev1.Pod) bool {
	ownerRef := metav1.GetControllerOf(pod)
	if ownerRef == nil {
		return true
	}
	pods, expectedReplicas, err := f.controllerFinder.GetPodsForRef(ownerRef, pod.Namespace, nil, false)
	if err != nil {
		return false
	}

	maxMigrating, err := util.GetMaxMigrating(int(expectedReplicas), f.args.MaxMigratingPerWorkload)
	if err != nil {
		return false
	}
	maxUnavailable, err := util.GetMaxUnavailable(int(expectedReplicas), f.args.MaxUnavailablePerWorkload)
	if err != nil {
		return false
	}

	var expectedPhaseAndAnnotations []PhaseAndAnnotation
	expectedPhaseAndAnnotations = []PhaseAndAnnotation{
		{sev1alpha1.PodMigrationJobRunning, nil},
		{sev1alpha1.PodMigrationJobPending, map[string]string{AnnotationPassedArbitration: "true"}},
	}

	opts := &client.ListOptions{FieldSelector: fields.OneTermEqualSelector(fieldindex.IndexJobByPodNamespace, pod.Namespace)}
	migratingPods := map[types.NamespacedName]struct{}{}
	f.forEachAvailableMigrationJobs(opts, func(job *sev1alpha1.PodMigrationJob) bool {
		podRef := job.Spec.PodRef
		if podRef == nil || podRef.UID == pod.UID {
			return true
		}

		podNamespacedName := types.NamespacedName{
			Namespace: podRef.Namespace,
			Name:      podRef.Name,
		}
		p := &corev1.Pod{}
		err := f.client.Get(context.TODO(), podNamespacedName, p)
		if err != nil {
			klog.Errorf("Failed to get Pod %q, err: %v", podNamespacedName, err)
		} else {
			innerPodOwnerRef := metav1.GetControllerOf(p)
			if innerPodOwnerRef != nil && innerPodOwnerRef.UID == ownerRef.UID {
				migratingPods[podNamespacedName] = struct{}{}
			}
		}
		return true
	}, expectedPhaseAndAnnotations...)

	if len(migratingPods) > 0 {
		exceeded := len(migratingPods) >= maxMigrating
		if exceeded {
			klog.V(4).Infof("The workload %s/%s/%s(%s) of Pod %q has %d migration jobs that exceed MaxMigratingPerWorkload %d",
				ownerRef.Name, ownerRef.Kind, ownerRef.APIVersion, ownerRef.UID, klog.KObj(pod), len(migratingPods), maxMigrating)
			return false
		}
	}

	unavailablePods := f.getUnavailablePods(pods)
	mergeUnavailableAndMigratingPods(unavailablePods, migratingPods)
	exceeded := len(unavailablePods) >= maxUnavailable
	if exceeded {
		klog.V(4).Infof("The workload %s/%s/%s(%s) of Pod %q has %d unavailable Pods that exceed MaxUnavailablePerWorkload %d",
			ownerRef.Name, ownerRef.Kind, ownerRef.APIVersion, ownerRef.UID, klog.KObj(pod), len(unavailablePods), maxUnavailable)
		return false
	}
	return true
}

func (f *filter) filterExpectedReplicas(pod *corev1.Pod) bool {
	ownerRef := metav1.GetControllerOf(pod)
	if ownerRef == nil {
		return true
	}
	_, expectedReplicas, err := f.controllerFinder.GetPodsForRef(ownerRef, pod.Namespace, nil, false)
	if err != nil {
		klog.Errorf("filterExpectedReplicas, getPodsForRef err: %s", err.Error())
		return false
	}

	maxMigrating, err := util.GetMaxMigrating(int(expectedReplicas), f.args.MaxMigratingPerWorkload)
	if err != nil {
		klog.Errorf("filterExpectedReplicas, getMaxMigrating err: %s", err.Error())
		return false
	}
	maxUnavailable, err := util.GetMaxUnavailable(int(expectedReplicas), f.args.MaxUnavailablePerWorkload)
	if err != nil {
		klog.Errorf("filterExpectedReplicas, getMaxUnavailable err: %s", err.Error())
		return false
	}
	if f.args.SkipCheckExpectedReplicas == nil || !*f.args.SkipCheckExpectedReplicas {
		// TODO(joseph): There are f few special scenarios where should we allow eviction?
		if expectedReplicas == 1 || int(expectedReplicas) == maxMigrating || int(expectedReplicas) == maxUnavailable {
			klog.Warningf("maxMigrating(%d) or maxUnavailable(%d) equals to the replicas(%d) of the workload %s/%s/%s(%s) of Pod %q, or the replicas equals to 1, please increase the replicas or update the defense configurations",
				maxMigrating, maxUnavailable, expectedReplicas, ownerRef.Name, ownerRef.Kind, ownerRef.APIVersion, ownerRef.UID, klog.KObj(pod))
			return false
		}
	}
	return true
}

func (f *filter) getUnavailablePods(pods []*corev1.Pod) map[types.NamespacedName]struct{} {
	unavailablePods := make(map[types.NamespacedName]struct{})
	for _, pod := range pods {
		if kubecontroller.IsPodActive(pod) && k8spodutil.IsPodReady(pod) {
			continue
		}
		k := types.NamespacedName{
			Namespace: pod.Namespace,
			Name:      pod.Name,
		}
		unavailablePods[k] = struct{}{}
	}
	return unavailablePods
}

func mergeUnavailableAndMigratingPods(unavailablePods, migratingPods map[types.NamespacedName]struct{}) {
	for k, v := range migratingPods {
		unavailablePods[k] = v
	}
}

func (f *filter) TrackEvictedPod(pod *corev1.Pod) {
	if f.objectLimiters == nil || f.limiterCache == nil {
		return
	}
	ownerRef := metav1.GetControllerOf(pod)
	if ownerRef == nil {
		return
	}

	objectLimiterArgs, ok := f.args.ObjectLimiters[deschedulerconfig.MigrationLimitObjectWorkload]
	if !ok || objectLimiterArgs.Duration.Seconds() == 0 {
		return
	}

	var maxMigratingReplicas int
	if expectedReplicas, err := f.controllerFinder.GetExpectedScaleForPod(pod); err == nil {
		maxMigrating := objectLimiterArgs.MaxMigrating
		if maxMigrating == nil {
			maxMigrating = f.args.MaxMigratingPerWorkload
		}
		maxMigratingReplicas, _ = util.GetMaxMigrating(int(expectedReplicas), maxMigrating)
	}
	if maxMigratingReplicas == 0 {
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	uid := ownerRef.UID
	limit := rate.Limit(maxMigratingReplicas) / rate.Limit(objectLimiterArgs.Duration.Seconds())
	limiter := f.objectLimiters[uid]
	if limiter == nil {
		limiter = rate.NewLimiter(limit, 1)
		f.objectLimiters[uid] = limiter
	} else if limiter.Limit() != limit {
		limiter.SetLimit(limit)
	}

	if !limiter.AllowN(f.clock.Now(), 1) {
		klog.Infof("The workload %s/%s/%s has been frequently descheduled recently and needs to be limited for f period of time", ownerRef.Name, ownerRef.Kind, ownerRef.APIVersion)
	}
	f.limiterCache.Set(string(uid), 0, gocache.DefaultExpiration)
}

func (f *filter) filterLimitedObject(pod *corev1.Pod) bool {
	if f.objectLimiters == nil || f.limiterCache == nil {
		return true
	}
	objectLimiterArgs, ok := f.args.ObjectLimiters[deschedulerconfig.MigrationLimitObjectWorkload]
	if !ok || objectLimiterArgs.Duration.Duration == 0 {
		return true
	}
	if ownerRef := metav1.GetControllerOf(pod); ownerRef != nil {
		f.mu.Lock()
		defer f.mu.Unlock()
		if limiter := f.objectLimiters[ownerRef.UID]; limiter != nil {
			if remainTokens := limiter.Tokens() - float64(1); remainTokens < 0 {
				klog.Infof("Pod %q is filtered by workload %s/%s/%s is limited", klog.KObj(pod), ownerRef.Name, ownerRef.Kind, ownerRef.APIVersion)
				return false
			}
		}
	}
	return true
}

func (f *filter) initObjectLimiters() {
	var trackExpiration time.Duration
	for _, v := range f.args.ObjectLimiters {
		if v.Duration.Duration > trackExpiration {
			trackExpiration = v.Duration.Duration
		}
	}
	if trackExpiration > 0 {
		f.objectLimiters = make(map[types.UID]*rate.Limiter)
		limiterExpiration := trackExpiration + trackExpiration/2
		f.limiterCache = gocache.New(limiterExpiration, limiterExpiration)
		f.limiterCache.OnEvicted(func(s string, _ interface{}) {
			f.mu.Lock()
			defer f.mu.Unlock()
			delete(f.objectLimiters, types.UID(s))
		})
	}
}

func isContain(a map[string]string, b map[string]string) bool {
	if b == nil {
		return true
	}
	if a == nil {
		if len(b) == 0 {
			return true
		} else {
			return false
		}
	} else {
		for k, v := range b {
			if a[k] != v {
				return false
			}
		}
		return true
	}
}

type PhaseAndAnnotation struct {
	phase       sev1alpha1.PodMigrationJobPhase
	annotations map[string]string
}
