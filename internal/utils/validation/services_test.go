// Copyright 2024
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package validation

import (
	"testing"

	kcmv1 "github.com/K0rdent/kcm/api/v1beta1"
	"github.com/stretchr/testify/require"
)

func TestValidateServiceDependency(t *testing.T) {
	for _, tc := range []struct {
		name        string
		services    []kcmv1.Service
		expectedErr string
	}{
		{
			name: "test1",
		},
		{
			name: "test2",
			services: []kcmv1.Service{
				{Namespace: "A", Name: "a"},
				{Namespace: "B", Name: "b"},
				{Namespace: "C", Name: "c"},
			},
		},
		{
			name: "test3",
			services: []kcmv1.Service{
				{Namespace: "A", Name: "a", DependsOn: []kcmv1.ServiceDependsOn{{Namespace: "C", Name: "c"}}},
				{Namespace: "B", Name: "b"},
			},
			expectedErr: "dependency C/c of service A/a is not defined as a service",
		},
		{
			name: "test4",
			services: []kcmv1.Service{
				{Namespace: "A", Name: "a", DependsOn: []kcmv1.ServiceDependsOn{{Namespace: "C", Name: "c"}, {Namespace: "D", Name: "d"}}},
				{Namespace: "B", Name: "b"},
			},
			expectedErr: "dependency C/c of service A/a is not defined as a service" +
				"\n" + "dependency D/d of service A/a is not defined as a service",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateServiceDependency(tc.services); err != nil {
				require.EqualError(t, err, tc.expectedErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestValidateServiceDependencyCycle(t *testing.T) {
	for _, tc := range []struct {
		testName string
		services []kcmv1.Service
		isErr    bool
	}{
		{
			testName: "test1",
			services: []kcmv1.Service{},
		},
		{
			testName: "test2",
			services: []kcmv1.Service{
				{
					Namespace: "A", Name: "a",
				},
			},
		},
		{
			testName: "test3",
			services: []kcmv1.Service{
				{
					Namespace: "A", Name: "a",
				},
				{
					Namespace: "A", Name: "a",
				},
			},
		},
		{
			testName: "test4",
			services: []kcmv1.Service{
				{
					Namespace: "A", Name: "a",
					DependsOn: []kcmv1.ServiceDependsOn{{Namespace: "B", Name: "b"}},
				},
				{
					Namespace: "B", Name: "b",
				},
			},
		},
		{
			testName: "test5",
			services: []kcmv1.Service{
				{
					Namespace: "A", Name: "a",
				},
				{
					Namespace: "B", Name: "b",
					DependsOn: []kcmv1.ServiceDependsOn{{Namespace: "A", Name: "a"}},
				},
			},
		},
		{
			testName: "test6",
			services: []kcmv1.Service{
				{
					Namespace: "A", Name: "a",
					DependsOn: []kcmv1.ServiceDependsOn{{Namespace: "A", Name: "a"}},
				},
			},
			isErr: true,
		},
		{
			testName: "test7",
			services: []kcmv1.Service{
				{
					Namespace: "A", Name: "a",
					DependsOn: []kcmv1.ServiceDependsOn{{Namespace: "B", Name: "b"}},
				},
				{
					Namespace: "B", Name: "b",
					DependsOn: []kcmv1.ServiceDependsOn{{Namespace: "A", Name: "a"}},
				},
			},
			isErr: true,
		},
		{
			testName: "test8",
			services: []kcmv1.Service{
				{
					Namespace: "A", Name: "a",
					DependsOn: []kcmv1.ServiceDependsOn{{Namespace: "B", Name: "b"}, {Namespace: "C", Name: "c"}},
				},
				{
					Namespace: "B", Name: "b",
					DependsOn: []kcmv1.ServiceDependsOn{{Namespace: "D", Name: "d"}, {Namespace: "E", Name: "e"}},
				},
				{
					Namespace: "C", Name: "c",
				},
				{
					Namespace: "D", Name: "d",
				},
				{
					Namespace: "E", Name: "e",
				},
			},
		},
		{
			testName: "test9",
			services: []kcmv1.Service{
				{
					Namespace: "A", Name: "a",
					DependsOn: []kcmv1.ServiceDependsOn{{Namespace: "B", Name: "b"}, {Namespace: "C", Name: "c"}},
				},
				{
					Namespace: "B", Name: "b",
					DependsOn: []kcmv1.ServiceDependsOn{{Namespace: "D", Name: "d"}, {Namespace: "E", Name: "e"}},
				},
				{
					Namespace: "C", Name: "c",
				},
				{
					Namespace: "D", Name: "d",
				},
				{
					Namespace: "E", Name: "e",
					DependsOn: []kcmv1.ServiceDependsOn{{Namespace: "A", Name: "a"}},
				},
			},
			isErr: true,
		},
		{
			testName: "test10",
			services: []kcmv1.Service{
				{
					Namespace: "C", Name: "c",
				},
				{
					Namespace: "A", Name: "a",
					DependsOn: []kcmv1.ServiceDependsOn{{Namespace: "B", Name: "b"}, {Namespace: "C", Name: "c"}},
				},
				{
					Namespace: "D", Name: "d",
				},
				{
					Namespace: "B", Name: "b",
					DependsOn: []kcmv1.ServiceDependsOn{{Namespace: "D", Name: "d"}, {Namespace: "E", Name: "e"}},
				},
				{
					Namespace: "E", Name: "e",
					DependsOn: []kcmv1.ServiceDependsOn{{Namespace: "A", Name: "a"}},
				},
			},
			isErr: true,
		},
	} {
		t.Run(tc.testName, func(t *testing.T) {
			err := ValidateServiceDependencyCycle(tc.services)
			if tc.isErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
