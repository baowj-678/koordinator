/*
Copyright 2022 The Koordinator Authors.
Copyright 2019 The Kubernetes Authors.

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

package topologymanager

import (
	"reflect"
	"testing"
)

func TestPolicySingleNumaNodeCanAdmitPodResult(t *testing.T) {
	tcases := []struct {
		name     string
		hint     NUMATopologyHint
		expected bool
	}{
		{
			name:     "Preferred is set to false in topology hints",
			hint:     NUMATopologyHint{nil, false},
			expected: false,
		},
	}

	for _, tc := range tcases {
		numaNodes := []int{0, 1}
		policy := NewSingleNumaNodePolicy(numaNodes)
		result := policy.(*singleNumaNodePolicy).canAdmitPodResult(&tc.hint)

		if result != tc.expected {
			t.Errorf("Expected result to be %t, got %t", tc.expected, result)
		}
	}
}

func TestPolicySingleNumaNodeFilterHints(t *testing.T) {
	tcases := []struct {
		name              string
		allResources      [][]NUMATopologyHint
		expectedResources [][]NUMATopologyHint
	}{
		{
			name:              "filter empty resources",
			allResources:      [][]NUMATopologyHint{},
			expectedResources: [][]NUMATopologyHint(nil),
		},
		{
			name: "filter hints with nil socket mask 1/2",
			allResources: [][]NUMATopologyHint{
				{
					{NUMANodeAffinity: nil, Preferred: false},
				},
				{
					{NUMANodeAffinity: nil, Preferred: true},
				},
			},
			expectedResources: [][]NUMATopologyHint{
				[]NUMATopologyHint(nil),
				{
					{NUMANodeAffinity: nil, Preferred: true},
				},
			},
		},
		{
			name: "filter hints with nil socket mask 2/2",
			allResources: [][]NUMATopologyHint{
				{
					{NUMANodeAffinity: NewTestBitMask(0), Preferred: true},
					{NUMANodeAffinity: nil, Preferred: false},
				},
				{
					{NUMANodeAffinity: NewTestBitMask(1), Preferred: true},
					{NUMANodeAffinity: nil, Preferred: true},
				},
			},
			expectedResources: [][]NUMATopologyHint{
				{
					{NUMANodeAffinity: NewTestBitMask(0), Preferred: true},
				},
				{
					{NUMANodeAffinity: NewTestBitMask(1), Preferred: true},
					{NUMANodeAffinity: nil, Preferred: true},
				},
			},
		},
		{
			name: "filter hints with empty resource socket mask",
			allResources: [][]NUMATopologyHint{
				{
					{NUMANodeAffinity: NewTestBitMask(1), Preferred: true},
					{NUMANodeAffinity: NewTestBitMask(0), Preferred: true},
					{NUMANodeAffinity: nil, Preferred: false},
				},
				{},
			},
			expectedResources: [][]NUMATopologyHint{
				{
					{NUMANodeAffinity: NewTestBitMask(1), Preferred: true},
					{NUMANodeAffinity: NewTestBitMask(0), Preferred: true},
				},
				[]NUMATopologyHint(nil),
			},
		},
		{
			name: "filter hints with wide sockemask",
			allResources: [][]NUMATopologyHint{
				{
					{NUMANodeAffinity: NewTestBitMask(0), Preferred: true},
					{NUMANodeAffinity: NewTestBitMask(1), Preferred: true},
					{NUMANodeAffinity: NewTestBitMask(1, 2), Preferred: false},
					{NUMANodeAffinity: NewTestBitMask(0, 1, 2), Preferred: false},
					{NUMANodeAffinity: nil, Preferred: false},
				},
				{
					{NUMANodeAffinity: NewTestBitMask(1, 2), Preferred: false},
					{NUMANodeAffinity: NewTestBitMask(0, 1, 2), Preferred: false},
					{NUMANodeAffinity: NewTestBitMask(0, 2), Preferred: false},
					{NUMANodeAffinity: NewTestBitMask(3), Preferred: false},
				},
				{
					{NUMANodeAffinity: NewTestBitMask(1, 2), Preferred: false},
					{NUMANodeAffinity: NewTestBitMask(0, 1, 2), Preferred: false},
					{NUMANodeAffinity: NewTestBitMask(0, 2), Preferred: false},
				},
			},
			expectedResources: [][]NUMATopologyHint{
				{
					{NUMANodeAffinity: NewTestBitMask(0), Preferred: true},
					{NUMANodeAffinity: NewTestBitMask(1), Preferred: true},
				},
				[]NUMATopologyHint(nil),
				[]NUMATopologyHint(nil),
			},
		},
	}

	for _, tc := range tcases {
		actual := filterSingleNumaHints(tc.allResources)
		if !reflect.DeepEqual(tc.expectedResources, actual) {
			t.Errorf("Test Case: %s", tc.name)
			t.Errorf("Expected result to be %v, got %v", tc.expectedResources, actual)
		}
	}
}

func TestPolicySingleNumaNodeMerge(t *testing.T) {
	numaNodes := []int{0, 1}
	policy := NewSingleNumaNodePolicy(numaNodes)

	tcases := commonPolicyMergeTestCases(numaNodes)
	tcases = append(tcases, policy.(*singleNumaNodePolicy).mergeTestCases(numaNodes)...)

	testPolicyMerge(policy, tcases, t)
}
