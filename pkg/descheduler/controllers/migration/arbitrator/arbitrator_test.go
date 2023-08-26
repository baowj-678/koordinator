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
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/koordinator-sh/koordinator/apis/extension"
	"github.com/koordinator-sh/koordinator/apis/scheduling/v1alpha1"
)

func TestSingleSortFn(t *testing.T) {
	creationTime := time.Now()
	pods := make([]*corev1.Pod, 20)
	jobs := make([]*v1alpha1.PodMigrationJob, len(pods))
	podOfJob := map[*v1alpha1.PodMigrationJob]*corev1.Pod{}
	expectedJobsOrder := make([]string, len(pods))
	for i := 0; i < len(pods); i++ {
		pods[i] = makePod("test-pod-"+strconv.Itoa(i+1), 0, extension.QoSNone, corev1.PodQOSBestEffort, creationTime)
		jobs[i] = makePodMigrationJob("test-job-"+strconv.Itoa(i+1), creationTime, pods[0])
		podOfJob[jobs[i]] = pods[i]
		expectedJobsOrder[i] = jobs[i].Name

	}
	scheme := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(scheme)
	_ = clientgoscheme.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	for _, pod := range pods {
		assert.Nil(t, fakeClient.Create(context.TODO(), pod))
	}
	collection := map[types.UID]*v1alpha1.PodMigrationJob{}
	for _, job := range collection {
		collection[job.UID] = job
	}
	arbitrator := &arbitratorImpl{
		waitingCollection: collection,
		client:            fakeClient,
		sorts: []SortFn{func(jobs []*v1alpha1.PodMigrationJob, podOfJob map[*v1alpha1.PodMigrationJob]*corev1.Pod) []*v1alpha1.PodMigrationJob {
			sort.SliceStable(jobs, func(i, j int) bool {
				return podOfJob[jobs[i]].Name < podOfJob[jobs[j]].Name
			})
			return jobs
		}},
	}
	arbitrator.sort(jobs, podOfJob)
	sort.SliceStable(expectedJobsOrder, func(i, j int) bool {
		return expectedJobsOrder[i] < expectedJobsOrder[j]
	})
	var jobsOrder []string
	for _, v := range jobs {
		jobsOrder = append(jobsOrder, v.Name)
	}
	assert.Equal(t, expectedJobsOrder, jobsOrder)
}

func TestMultiSortFn(t *testing.T) {
	creationTime := time.Now()
	pods := make([]*corev1.Pod, 20)
	jobs := make([]*v1alpha1.PodMigrationJob, len(pods))
	podOfJob := map[*v1alpha1.PodMigrationJob]*corev1.Pod{}
	for i := 0; i < len(pods); i++ {
		pods[i] = makePod("test-pod-"+strconv.Itoa(i+1), 0, extension.QoSNone, corev1.PodQOSBestEffort, creationTime)
		jobs[i] = makePodMigrationJob("test-job-"+strconv.Itoa(i+1), creationTime, pods[0])
		podOfJob[jobs[i]] = pods[i]

	}
	scheme := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(scheme)
	_ = clientgoscheme.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	for _, pod := range pods {
		assert.Nil(t, fakeClient.Create(context.TODO(), pod))
	}
	collection := map[types.UID]*v1alpha1.PodMigrationJob{}
	for _, job := range collection {
		collection[job.UID] = job
	}
	arbitrator := &arbitratorImpl{
		waitingCollection: collection,
		client:            fakeClient,
		sorts: []SortFn{
			func(jobs []*v1alpha1.PodMigrationJob, podOfJob map[*v1alpha1.PodMigrationJob]*corev1.Pod) []*v1alpha1.PodMigrationJob {
				sort.SliceStable(jobs, func(i, j int) bool {
					return podOfJob[jobs[i]].Name < podOfJob[jobs[j]].Name
				})
				return jobs
			},
			func(jobs []*v1alpha1.PodMigrationJob, podOfJob map[*v1alpha1.PodMigrationJob]*corev1.Pod) []*v1alpha1.PodMigrationJob {
				sort.SliceStable(jobs, func(i, j int) bool {
					return podOfJob[jobs[i]].Name[len(podOfJob[jobs[i]].Name)-1] < podOfJob[jobs[j]].Name[len(podOfJob[jobs[j]].Name)-1]
				})
				return jobs
			},
		},
	}
	expectedJobsOrder := []string{"test-job-10", "test-job-20", "test-job-1", "test-job-11", "test-job-12", "test-job-2", "test-job-13", "test-job-3", "test-job-14", "test-job-4", "test-job-15", "test-job-5", "test-job-16", "test-job-6", "test-job-17", "test-job-7", "test-job-18", "test-job-8", "test-job-19", "test-job-9"}
	arbitrator.sort(jobs, podOfJob)
	var jobsOrder []string
	for _, v := range jobs {
		jobsOrder = append(jobsOrder, v.Name)
	}
	assert.Equal(t, expectedJobsOrder, jobsOrder)
}

func TestFilter(t *testing.T) {
	testCases := []struct {
		name            string
		jobNum          int
		nonRetryableMap map[int]bool
		retryableMap    map[int]bool

		expectWaitCollection map[int]bool
		expectQueue          map[int]bool
	}{
		{
			name:            "test-1",
			jobNum:          10,
			nonRetryableMap: map[int]bool{2: true},
			retryableMap:    map[int]bool{3: true, 7: true},

			expectWaitCollection: map[int]bool{3: true, 7: true},
			expectQueue:          map[int]bool{0: true, 1: true, 4: true, 5: true, 6: true, 8: true, 9: true},
		},
		{
			name:            "test-2",
			jobNum:          10,
			nonRetryableMap: map[int]bool{3: true},
			retryableMap:    map[int]bool{3: true, 7: true},

			expectWaitCollection: map[int]bool{7: true},
			expectQueue:          map[int]bool{0: true, 1: true, 2: true, 4: true, 5: true, 6: true, 8: true, 9: true},
		},
		{
			name:            "test-3",
			jobNum:          10,
			nonRetryableMap: map[int]bool{},
			retryableMap:    map[int]bool{},

			expectWaitCollection: map[int]bool{},
			expectQueue:          map[int]bool{0: true, 1: true, 2: true, 3: true, 4: true, 5: true, 6: true, 7: true, 8: true, 9: true},
		},
		{
			name:            "test-4",
			jobNum:          5,
			nonRetryableMap: map[int]bool{0: true, 1: true, 2: true, 3: true, 4: true},
			retryableMap:    map[int]bool{},

			expectWaitCollection: map[int]bool{},
			expectQueue:          map[int]bool{},
		},
		{
			name:            "test-5",
			jobNum:          5,
			nonRetryableMap: map[int]bool{},
			retryableMap:    map[int]bool{0: true, 1: true, 2: true, 3: true, 4: true},

			expectWaitCollection: map[int]bool{0: true, 1: true, 2: true, 3: true, 4: true},
			expectQueue:          map[int]bool{},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			_ = v1alpha1.AddToScheme(scheme)
			_ = clientgoscheme.AddToScheme(scheme)
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

			jobs := make([]*v1alpha1.PodMigrationJob, testCase.jobNum)
			podOfJob := map[*v1alpha1.PodMigrationJob]*corev1.Pod{}
			nonRetryablePods := map[*corev1.Pod]bool{}
			var nonRetryableJobs []*v1alpha1.PodMigrationJob
			retryablePods := map[*corev1.Pod]bool{}
			var expectWorkQueueJob []*v1alpha1.PodMigrationJob
			var expectWorkQueue []string
			collection := map[types.UID]*v1alpha1.PodMigrationJob{}
			var expectWaitCollection []types.UID

			for i := range jobs {
				pod := makePod("test-pod-"+strconv.Itoa(i), 0, extension.QoSNone, corev1.PodQOSBestEffort, time.Now())
				jobs[i] = makePodMigrationJob("test-job-"+strconv.Itoa(i), time.Now(), pod)
				assert.Nil(t, fakeClient.Create(context.TODO(), pod))
				assert.Nil(t, fakeClient.Create(context.TODO(), jobs[i]))
				podOfJob[jobs[i]] = pod
				if testCase.nonRetryableMap[i] {
					nonRetryablePods[pod] = true
					nonRetryableJobs = append(nonRetryableJobs, jobs[i])
				}
				if testCase.retryableMap[i] {
					retryablePods[pod] = true
				}
				if testCase.expectWaitCollection[i] {
					expectWaitCollection = append(expectWaitCollection, jobs[i].UID)
				}
				if testCase.expectQueue[i] {
					expectWorkQueueJob = append(expectWorkQueueJob, jobs[i])
					expectWorkQueue = append(expectWorkQueue, jobs[i].Name)
				}
				collection[jobs[i].UID] = jobs[i]
			}
			a := &arbitratorImpl{
				waitingCollection: collection,
				workQueue:         workqueue.NewRateLimitingQueue(&workqueue.BucketRateLimiter{Limiter: rate.NewLimiter(100, 100)}),
				nonRetryablePodFilter: func(pod *corev1.Pod) bool {
					return !nonRetryablePods[pod]
				},
				retryablePodFilter: func(pod *corev1.Pod) bool {
					return !retryablePods[pod]
				},
				client:        fakeClient,
				mu:            sync.Mutex{},
				eventRecorder: fakeEventRecord{},
				interval:      0,
			}

			a.filter(jobs, podOfJob)

			var actualWaitCollection []types.UID
			for uid := range a.waitingCollection {
				actualWaitCollection = append(actualWaitCollection, uid)
			}
			assert.ElementsMatchf(t, actualWaitCollection, expectWaitCollection, "waitingCollection")

			var actualWorkQueue []string
			cnt := 0
			for {
				if cnt >= len(expectWorkQueueJob) {
					break
				}
				item, _ := a.workQueue.Get()
				actualWorkQueue = append(actualWorkQueue, item.(reconcile.Request).Name)
				cnt++
			}
			assert.ElementsMatchf(t, actualWorkQueue, expectWorkQueue, "workQueue")

			for _, job := range nonRetryableJobs {
				assert.Nil(t, fakeClient.Get(context.TODO(), types.NamespacedName{
					Namespace: job.Namespace,
					Name:      job.Name,
				}, job))
				assert.Equal(t, v1alpha1.PodMigrationJobFailed, job.Status.Phase)
			}

			for _, job := range expectWorkQueueJob {
				assert.Nil(t, fakeClient.Get(context.TODO(), types.NamespacedName{
					Namespace: job.Namespace,
					Name:      job.Name,
				}, job))
				assert.Equal(t, "true", job.Annotations[AnnotationPassedArbitration])
			}
		})
	}
}

func TestAdd(t *testing.T) {
	creationTime := time.Now()
	migratingJobs := []*v1alpha1.PodMigrationJob{
		makePodMigrationJob("test-job-1", creationTime, nil),
		makePodMigrationJob("test-job-2", creationTime, nil),
		makePodMigrationJob("test-job-3", creationTime, nil),
		makePodMigrationJob("test-job-4", creationTime, nil),
		makePodMigrationJob("test-job-5", creationTime, nil),
	}
	scheme := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(scheme)
	_ = clientgoscheme.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	arbitrator := &arbitratorImpl{
		waitingCollection: map[types.UID]*v1alpha1.PodMigrationJob{},
		workQueue:         workqueue.NewRateLimitingQueue(&workqueue.BucketRateLimiter{Limiter: rate.NewLimiter(1, 1)}),
		client:            fakeClient,
	}

	for _, job := range migratingJobs {
		arbitrator.Add(job)
	}

	var actualJobs []string
	for _, job := range arbitrator.waitingCollection {
		actualJobs = append(actualJobs, job.Name)
	}
	expectedJobs := []string{"test-job-1", "test-job-2", "test-job-3", "test-job-4", "test-job-5"}
	assert.ElementsMatchf(t, expectedJobs, actualJobs, "failed")
}

func TestRequeueJobIfRetryablePodFilterFailed(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(scheme)
	_ = clientgoscheme.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	job := &v1alpha1.PodMigrationJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test",
			CreationTimestamp: metav1.Time{Time: time.Now()},
		},
		Spec: v1alpha1.PodMigrationJobSpec{
			PodRef: &corev1.ObjectReference{
				Namespace: "default",
				Name:      "test-pod",
			},
		},
	}
	assert.Nil(t, fakeClient.Create(context.TODO(), job))
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "test-pod",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Controller: pointer.Bool(true),
					Kind:       "StatefulSet",
					Name:       "test",
					UID:        "2f96233d-a6b9-4981-b594-7c90c987aed9",
				},
			},
		},
		Spec: corev1.PodSpec{
			SchedulerName: "koord-scheduler",
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}
	assert.Nil(t, fakeClient.Create(context.TODO(), pod))
	enter := false

	a := &arbitratorImpl{
		waitingCollection: map[types.UID]*v1alpha1.PodMigrationJob{job.UID: job},
		workQueue:         workqueue.NewRateLimitingQueue(&workqueue.BucketRateLimiter{Limiter: rate.NewLimiter(100, 100)}),
		sorts: []SortFn{func(jobs []*v1alpha1.PodMigrationJob, podOfJob map[*v1alpha1.PodMigrationJob]*corev1.Pod) []*v1alpha1.PodMigrationJob {
			return jobs
		}},
		nonRetryablePodFilter: func(pod *corev1.Pod) bool {
			return true
		},
		retryablePodFilter: func(pod *corev1.Pod) bool {
			enter = true
			return false
		},
		client:        fakeClient,
		mu:            sync.Mutex{},
		eventRecorder: fakeEventRecord{},
		interval:      0,
	}

	a.doOnceArbitrate()

	assert.True(t, enter)
	assert.Equal(t, 1, len(a.waitingCollection))
	assert.NoError(t, fakeClient.Get(context.TODO(), types.NamespacedName{Name: job.Name}, job))
	assert.Equal(t, v1alpha1.PodMigrationJobPhase(""), job.Status.Phase)
	assert.Equal(t, 0, len(job.Annotations))
	assert.Equal(t, "", job.Status.Reason)
}

func TestAbortJobIfNonRetryablePodFilterFailed(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(scheme)
	_ = clientgoscheme.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	job := &v1alpha1.PodMigrationJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test",
			CreationTimestamp: metav1.Time{Time: time.Now()},
		},
		Spec: v1alpha1.PodMigrationJobSpec{
			PodRef: &corev1.ObjectReference{
				Namespace: "default",
				Name:      "test-pod",
			},
		},
	}
	assert.Nil(t, fakeClient.Create(context.TODO(), job))
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "test-pod",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Controller: pointer.Bool(true),
					Kind:       "StatefulSet",
					Name:       "test",
					UID:        "2f96233d-a6b9-4981-b594-7c90c987aed9",
				},
			},
		},
		Spec: corev1.PodSpec{
			SchedulerName: "koord-scheduler",
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}
	assert.Nil(t, fakeClient.Create(context.TODO(), pod))
	enter := false

	a := &arbitratorImpl{
		waitingCollection: map[types.UID]*v1alpha1.PodMigrationJob{job.UID: job},
		workQueue:         workqueue.NewRateLimitingQueue(&workqueue.BucketRateLimiter{Limiter: rate.NewLimiter(100, 100)}),
		sorts: []SortFn{func(jobs []*v1alpha1.PodMigrationJob, podOfJob map[*v1alpha1.PodMigrationJob]*corev1.Pod) []*v1alpha1.PodMigrationJob {
			return jobs
		}},
		nonRetryablePodFilter: func(pod *corev1.Pod) bool {
			enter = true
			return false
		},
		retryablePodFilter: func(pod *corev1.Pod) bool {
			return true
		},
		client:        fakeClient,
		mu:            sync.Mutex{},
		eventRecorder: fakeEventRecord{},
		interval:      0,
	}

	a.doOnceArbitrate()

	assert.True(t, enter)

	assert.NoError(t, fakeClient.Get(context.TODO(), types.NamespacedName{Name: job.Name}, job))
	assert.Equal(t, v1alpha1.PodMigrationJobFailed, job.Status.Phase)
	assert.Equal(t, v1alpha1.PodMigrationJobReasonForbiddenMigratePod, job.Status.Reason)
}

func TestDoOnceArbitrate(t *testing.T) {
	testCases := []struct {
		name            string
		jobNum          int
		nonRetryableMap map[int]bool
		retryableMap    map[int]bool
		order           map[int]int

		expectWaitCollection map[int]bool
		expectQueue          map[int]int
	}{
		{
			name:            "test-1",
			jobNum:          10,
			nonRetryableMap: map[int]bool{2: true},
			retryableMap:    map[int]bool{3: true, 7: true},
			order:           map[int]int{0: 0, 1: 1, 2: 2, 3: 3, 4: 4, 5: 5, 6: 6, 7: 7, 8: 8, 9: 9},

			expectWaitCollection: map[int]bool{3: true, 7: true},
			expectQueue:          map[int]int{0: 1, 1: 2, 4: 3, 5: 4, 6: 5, 8: 6, 9: 7},
		},
		{
			name:            "test-2",
			jobNum:          10,
			nonRetryableMap: map[int]bool{3: true},
			retryableMap:    map[int]bool{3: true, 7: true},
			order:           map[int]int{0: 9, 1: 8, 2: 7, 3: 6, 4: 5, 5: 4, 6: 3, 7: 2, 8: 1, 9: 0},

			expectWaitCollection: map[int]bool{7: true},
			expectQueue:          map[int]int{0: 8, 1: 7, 2: 6, 4: 5, 5: 4, 6: 3, 8: 2, 9: 1},
		},
		{
			name:            "test-3",
			jobNum:          5,
			nonRetryableMap: map[int]bool{0: true, 1: true, 2: true, 3: true, 4: true},
			retryableMap:    map[int]bool{},

			expectWaitCollection: map[int]bool{},
			expectQueue:          map[int]int{},
		},
		{
			name:            "test-4",
			jobNum:          5,
			nonRetryableMap: map[int]bool{},
			retryableMap:    map[int]bool{0: true, 1: true, 2: true, 3: true, 4: true},
			order:           map[int]int{0: 0, 1: 3, 2: 1, 3: 4, 4: 2},

			expectWaitCollection: map[int]bool{0: true, 1: true, 2: true, 3: true, 4: true},
			expectQueue:          map[int]int{},
		},
		{
			name:            "test-5",
			jobNum:          5,
			nonRetryableMap: map[int]bool{},
			retryableMap:    map[int]bool{2: true},
			order:           map[int]int{0: 0, 1: 3, 2: 1, 3: 4, 4: 2},

			expectWaitCollection: map[int]bool{2: true},
			expectQueue:          map[int]int{0: 1, 1: 3, 3: 4, 4: 2},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			_ = v1alpha1.AddToScheme(scheme)
			_ = clientgoscheme.AddToScheme(scheme)
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

			jobs := make([]*v1alpha1.PodMigrationJob, testCase.jobNum)
			podOfJob := map[*v1alpha1.PodMigrationJob]*corev1.Pod{}
			nonRetryablePods := map[string]bool{}
			retryablePods := map[string]bool{}
			expectWorkQueue := map[string]int{}
			collection := map[types.UID]*v1alpha1.PodMigrationJob{}
			var expectWaitCollection []types.UID
			order := map[*v1alpha1.PodMigrationJob]int{}

			for i := range jobs {
				pod := makePod("test-pod-"+strconv.Itoa(i), 0, extension.QoSNone, corev1.PodQOSBestEffort, time.Now())
				jobs[i] = makePodMigrationJob("test-job-"+strconv.Itoa(i), time.Now(), pod)
				assert.Nil(t, fakeClient.Create(context.TODO(), pod))
				assert.Nil(t, fakeClient.Create(context.TODO(), jobs[i]))
				podOfJob[jobs[i]] = pod
				if testCase.nonRetryableMap[i] {
					nonRetryablePods[pod.Name] = true
				}
				if testCase.retryableMap[i] {
					retryablePods[pod.Name] = true
				}
				if testCase.expectWaitCollection[i] {
					expectWaitCollection = append(expectWaitCollection, jobs[i].UID)
				}
				if v, ok := testCase.expectQueue[i]; ok {
					expectWorkQueue[jobs[i].Name] = v
				}
				order[jobs[i]] = testCase.order[i]
				collection[jobs[i].UID] = jobs[i]
			}
			a := &arbitratorImpl{
				waitingCollection: collection,
				workQueue:         workqueue.NewRateLimitingQueue(&workqueue.BucketRateLimiter{Limiter: rate.NewLimiter(100, 100)}),
				nonRetryablePodFilter: func(pod *corev1.Pod) bool {
					return !nonRetryablePods[pod.Name]
				},
				retryablePodFilter: func(pod *corev1.Pod) bool {
					return !retryablePods[pod.Name]
				},
				sorts: []SortFn{
					func(jobs []*v1alpha1.PodMigrationJob, podOfJob map[*v1alpha1.PodMigrationJob]*corev1.Pod) []*v1alpha1.PodMigrationJob {
						sort.Slice(jobs, func(i, j int) bool {
							return order[jobs[i]] < order[jobs[j]]
						})
						return jobs
					}},
				client:        fakeClient,
				mu:            sync.Mutex{},
				eventRecorder: fakeEventRecord{},
				interval:      0,
			}

			a.doOnceArbitrate()

			var actualWaitCollection []types.UID
			for uid := range a.waitingCollection {
				actualWaitCollection = append(actualWaitCollection, uid)
			}
			assert.ElementsMatchf(t, actualWaitCollection, expectWaitCollection, "waitingCollection")

			actualWorkQueue := map[string]int{}
			cnt := 0
			for {
				if cnt >= len(expectWorkQueue) {
					break
				}
				cnt++
				item, _ := a.workQueue.Get()
				actualWorkQueue[item.(reconcile.Request).Name] = cnt
			}
			assert.Equal(t, expectWorkQueue, actualWorkQueue, "workQueue")
		})
	}
}

func TestArbitrate(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(scheme)
	_ = clientgoscheme.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	a := &arbitratorImpl{
		waitingCollection: map[types.UID]*v1alpha1.PodMigrationJob{},
		workQueue:         workqueue.NewRateLimitingQueue(&workqueue.BucketRateLimiter{Limiter: rate.NewLimiter(100, 100)}),
		nonRetryablePodFilter: func(pod *corev1.Pod) bool {
			return true
		},
		retryablePodFilter: func(pod *corev1.Pod) bool {
			return true
		},
		sorts: []SortFn{
			func(jobs []*v1alpha1.PodMigrationJob, podOfJob map[*v1alpha1.PodMigrationJob]*corev1.Pod) []*v1alpha1.PodMigrationJob {
				return jobs
			}},
		client:        fakeClient,
		mu:            sync.Mutex{},
		eventRecorder: fakeEventRecord{},
		interval:      500,
	}

	ch := make(<-chan struct{})
	go a.Arbitrate(ch)

	for i := 0; i < 5; i++ {
		pod := makePod("test-pod-"+strconv.Itoa(i), 0, extension.QoSNone, corev1.PodQOSBestEffort, time.Now())
		job := makePodMigrationJob("test-job-"+strconv.Itoa(i), time.Now(), pod)
		assert.Nil(t, fakeClient.Create(context.TODO(), pod))
		assert.Nil(t, fakeClient.Create(context.TODO(), job))
		a.Add(job)
		time.Sleep(800 * time.Millisecond)
		actualName, _ := a.workQueue.Get()
		assert.Equal(t, job.Name, actualName.(reconcile.Request).Name)
	}
}

func TestUpdatePassedJob(t *testing.T) {
	job := &v1alpha1.PodMigrationJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test",
			Namespace: "default",
			UID:       "test-uid",
		},
	}
	scheme := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(scheme)
	_ = clientgoscheme.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(job).Build()
	arbitrator := &arbitratorImpl{
		waitingCollection: map[types.UID]*v1alpha1.PodMigrationJob{job.UID: job},
		client:            fakeClient,
		mu:                sync.Mutex{},
		eventRecorder:     fakeEventRecord{},
		workQueue:         workqueue.NewRateLimitingQueue(&workqueue.BucketRateLimiter{Limiter: rate.NewLimiter(1, 1)}),
	}
	arbitrator.updatePassedJob(job)

	assert.Equal(t, 0, len(arbitrator.waitingCollection))

	actualJob := &v1alpha1.PodMigrationJob{}
	assert.Nil(t, fakeClient.Get(context.TODO(), types.NamespacedName{
		Namespace: "default",
		Name:      "test",
	}, actualJob))
	assert.Equal(t, map[string]string{AnnotationPassedArbitration: "true"}, actualJob.Annotations)
}

func TestUpdateFailedJob(t *testing.T) {
	job := &v1alpha1.PodMigrationJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test",
			Namespace: "default",
			UID:       "test-uid",
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
		},
	}
	scheme := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(scheme)
	_ = clientgoscheme.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(job).Build()
	arbitrator := &arbitratorImpl{
		waitingCollection: map[types.UID]*v1alpha1.PodMigrationJob{job.UID: job},
		client:            fakeClient,
		mu:                sync.Mutex{},
		eventRecorder:     fakeEventRecord{},
	}
	arbitrator.updateFailedJob(job, pod)

	assert.Equal(t, 0, len(arbitrator.waitingCollection))

	actualJob := &v1alpha1.PodMigrationJob{}
	assert.Nil(t, fakeClient.Get(context.TODO(), types.NamespacedName{
		Namespace: "default",
		Name:      "test",
	}, actualJob))
	assert.Equal(t, v1alpha1.PodMigrationJobFailed, actualJob.Status.Phase)
}

func TestEventHandlerCreate(t *testing.T) {
	creationTime := time.Now()
	migratingJobs := []*v1alpha1.PodMigrationJob{
		makePodMigrationJob("test-job-1", creationTime, nil),
		makePodMigrationJob("test-job-2", creationTime, nil),
		makePodMigrationJob("test-job-3", creationTime, nil),
		makePodMigrationJob("test-job-4", creationTime, nil),
		makePodMigrationJob("test-job-5", creationTime, nil),
	}
	scheme := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(scheme)
	_ = clientgoscheme.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	queue := workqueue.NewRateLimitingQueue(&workqueue.BucketRateLimiter{Limiter: rate.NewLimiter(1, 1)})

	arbitrator := &arbitratorImpl{
		waitingCollection: map[types.UID]*v1alpha1.PodMigrationJob{},
		workQueue:         nil,
		client:            fakeClient,
	}

	var expectedJobs []string
	for _, job := range migratingJobs {
		assert.Nil(t, fakeClient.Create(context.TODO(), job))

		arbitrator.Create(event.CreateEvent{Object: job}, queue)
		expectedJobs = append(expectedJobs, job.Name)
		assert.Equal(t, arbitrator.workQueue, queue)

		var actualJobs []string
		for _, v := range arbitrator.waitingCollection {
			actualJobs = append(actualJobs, v.Name)
		}
		assert.ElementsMatch(t, actualJobs, expectedJobs)
	}
	assert.Equal(t, 0, queue.Len())
	nilJob := makePodMigrationJob("test-job-6", creationTime, nil)
	arbitrator.Create(event.CreateEvent{Object: nilJob}, queue)

	actualJob, _ := queue.Get()
	assert.Equal(t, actualJob.(reconcile.Request).Name, nilJob.Name)
}

func TestEventHandler(t *testing.T) {
	testCases := []string{"Update", "Delete", "Generic"}
	for _, testCase := range testCases {
		t.Run(testCase, func(t *testing.T) {
			creationTime := time.Now()
			migratingJobs := []*v1alpha1.PodMigrationJob{
				makePodMigrationJob("test-job-1", creationTime, nil),
				makePodMigrationJob("test-job-2", creationTime, nil),
				makePodMigrationJob("test-job-3", creationTime, nil),
				makePodMigrationJob("test-job-4", creationTime, nil),
				makePodMigrationJob("test-job-5", creationTime, nil),
			}
			scheme := runtime.NewScheme()
			_ = v1alpha1.AddToScheme(scheme)
			_ = clientgoscheme.AddToScheme(scheme)
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

			queue := workqueue.NewRateLimitingQueue(&workqueue.BucketRateLimiter{Limiter: rate.NewLimiter(1, 1)})

			arbitrator := &arbitratorImpl{
				waitingCollection: map[types.UID]*v1alpha1.PodMigrationJob{},
				workQueue:         nil,
				client:            fakeClient,
			}
			for _, job := range migratingJobs {
				switch testCase {
				case "Update":
					arbitrator.Update(event.UpdateEvent{ObjectNew: job}, queue)
				case "Delete":
					arbitrator.Delete(event.DeleteEvent{Object: job}, queue)
				case "Generic":
					arbitrator.Generic(event.GenericEvent{Object: job}, queue)
				}

				actualJob, _ := queue.Get()
				assert.Equal(t, actualJob.(reconcile.Request).Name, job.Name)
			}
		})
	}
}

type podDecoratorFn func(pod *corev1.Pod)

type jobDecoratorFn func(job *v1alpha1.PodMigrationJob)

func makePod(name string, priority int32, koordQoS extension.QoSClass, k8sQoS corev1.PodQOSClass, creationTime time.Time, decoratorFns ...podDecoratorFn) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels: map[string]string{
				extension.LabelPodQoS: string(koordQoS),
			},
			Annotations:       map[string]string{},
			CreationTimestamp: metav1.Time{Time: creationTime},
			UID:               types.UID("default" + "/" + name),
		},
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "cores/v1",
		},
		Spec: corev1.PodSpec{
			NodeName: "test-node",
			Priority: &priority,
		},
		Status: corev1.PodStatus{
			QOSClass: k8sQoS,
		},
	}
	for _, decorator := range decoratorFns {
		decorator(pod)
	}
	return pod
}

func makePodMigrationJob(name string, creationTime time.Time, pod *corev1.Pod, decoratorFns ...jobDecoratorFn) *v1alpha1.PodMigrationJob {
	job := &v1alpha1.PodMigrationJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "default",
			CreationTimestamp: metav1.Time{Time: creationTime},
			UID:               types.UID(name + "uid"),
		},
	}
	if pod != nil {
		job.Spec.PodRef = &corev1.ObjectReference{
			Kind:            pod.Kind,
			Namespace:       pod.Namespace,
			Name:            pod.Name,
			UID:             pod.UID,
			APIVersion:      pod.APIVersion,
			ResourceVersion: pod.ResourceVersion,
		}
	}
	for _, decorator := range decoratorFns {
		decorator(job)
	}
	return job
}

type fakeEventRecord struct{}

func (f fakeEventRecord) Eventf(regarding runtime.Object, related runtime.Object, eventtype, reason, action, note string, args ...interface{}) {
}
