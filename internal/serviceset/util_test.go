// Copyright 2025
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

package serviceset

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kcmv1 "github.com/K0rdent/kcm/api/v1beta1"
)

func Test_ServicesToDeploy(t *testing.T) {
	t.Parallel()

	type testCase struct {
		description      string
		upgradePaths     []kcmv1.ServiceUpgradePaths
		desiredServices  []kcmv1.Service
		deployedServices []kcmv1.ServiceWithValues
		expectedServices []kcmv1.ServiceWithValues
	}

	f := func(t *testing.T, tc testCase) {
		t.Helper()
		actualServices := ServicesToDeploy(tc.upgradePaths, tc.desiredServices, tc.deployedServices)
		assert.ElementsMatch(t, tc.expectedServices, actualServices)
	}

	cases := []testCase{
		{
			description: "all-service-to-deploy",
			upgradePaths: []kcmv1.ServiceUpgradePaths{
				{
					Name:      "service1",
					Namespace: metav1.NamespaceDefault,
					Template:  "template1-1-0-0",
					AvailableUpgrades: []kcmv1.UpgradePath{
						{
							Versions: []string{"template1-1-0-0"},
						},
					},
				},
				{
					Name:      "service2",
					Namespace: metav1.NamespaceDefault,
					Template:  "template2-1-0-0",
					AvailableUpgrades: []kcmv1.UpgradePath{
						{
							Versions: []string{"template2-1-0-0"},
						},
					},
				},
			},
			desiredServices: []kcmv1.Service{
				{
					Name:      "service1",
					Namespace: metav1.NamespaceDefault,
					Template:  "template1-1-0-0",
				},
				{
					Name:      "service2",
					Namespace: metav1.NamespaceDefault,
					Template:  "template2-1-0-0",
				},
			},
			deployedServices: []kcmv1.ServiceWithValues{},
			expectedServices: []kcmv1.ServiceWithValues{
				{
					Name:      "service1",
					Namespace: metav1.NamespaceDefault,
					Template:  "template1-1-0-0",
				},
				{
					Name:      "service2",
					Namespace: metav1.NamespaceDefault,
					Template:  "template2-1-0-0",
				},
			},
		},
		{
			description: "service-to-be-upgraded",
			upgradePaths: []kcmv1.ServiceUpgradePaths{
				{
					Name:      "service1",
					Namespace: metav1.NamespaceDefault,
					Template:  "template1-1-0-0",
					AvailableUpgrades: []kcmv1.UpgradePath{
						{
							Versions: []string{"template1-1-5-0"},
						},
					},
				},
				{
					Name:      "service2",
					Namespace: metav1.NamespaceDefault,
					Template:  "template2-1-0-0",
					AvailableUpgrades: []kcmv1.UpgradePath{
						{
							Versions: []string{"template2-1-0-0"},
						},
					},
				},
			},
			desiredServices: []kcmv1.Service{
				{
					Name:      "service1",
					Namespace: metav1.NamespaceDefault,
					Template:  "template1-1-5-0",
				},
				{
					Name:      "service2",
					Namespace: metav1.NamespaceDefault,
					Template:  "template2-1-0-0",
				},
			},
			deployedServices: []kcmv1.ServiceWithValues{
				{
					Name:      "service1",
					Namespace: metav1.NamespaceDefault,
					Template:  "template1-1-0-0",
				},
				{
					Name:      "service2",
					Namespace: metav1.NamespaceDefault,
					Template:  "template2-1-0-0",
				},
			},
			expectedServices: []kcmv1.ServiceWithValues{
				{
					Name:      "service1",
					Namespace: metav1.NamespaceDefault,
					Template:  "template1-1-5-0",
				},
				{
					Name:      "service2",
					Namespace: metav1.NamespaceDefault,
					Template:  "template2-1-0-0",
				},
			},
		},
		{
			description: "service-should-not-be-upgraded",
			upgradePaths: []kcmv1.ServiceUpgradePaths{
				{
					Name:      "service1",
					Namespace: metav1.NamespaceDefault,
					Template:  "template1-1-0-0",
					AvailableUpgrades: []kcmv1.UpgradePath{
						{
							Versions: []string{"template1-1-5-0"},
						},
					},
				},
				{
					Name:      "service2",
					Namespace: metav1.NamespaceDefault,
					Template:  "template2-1-0-0",
					AvailableUpgrades: []kcmv1.UpgradePath{
						{
							Versions: []string{"template2-1-0-0"},
						},
					},
				},
			},
			desiredServices: []kcmv1.Service{
				{
					Name:      "service1",
					Namespace: metav1.NamespaceDefault,
					Template:  "template1-1-5-0",
				},
				{
					Name:      "service2",
					Namespace: metav1.NamespaceDefault,
					Template:  "template2-2-0-0",
				},
			},
			deployedServices: []kcmv1.ServiceWithValues{
				{
					Name:      "service1",
					Namespace: metav1.NamespaceDefault,
					Template:  "template1-1-0-0",
				},
				{
					Name:      "service2",
					Namespace: metav1.NamespaceDefault,
					Template:  "template2-1-0-0",
				},
			},
			expectedServices: []kcmv1.ServiceWithValues{
				{
					Name:      "service1",
					Namespace: metav1.NamespaceDefault,
					Template:  "template1-1-5-0",
				},
				{
					Name:      "service2",
					Namespace: metav1.NamespaceDefault,
					Template:  "template2-1-0-0",
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.description, func(t *testing.T) {
			t.Parallel()
			f(t, tc)
		})
	}
}

func Test_FilterServiceDependencies(t *testing.T) {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(kcmv1.AddToScheme(scheme))

	getRelevantFields := func(services []kcmv1.Service) []map[client.ObjectKey]struct{} {
		t.Helper()
		result := make([]map[client.ObjectKey]struct{}, len(services))
		for i, svc := range services {
			result[i] = map[client.ObjectKey]struct{}{
				ServiceKey(svc.Namespace, svc.Name): {},
			}
		}
		return result
	}

	for _, tc := range []struct {
		testName        string
		cdNamespace     string
		cdName          string
		desiredServices []kcmv1.Service
		objects         []client.Object
		expected        []kcmv1.Service
	}{
		{
			testName:    "test0",
			cdNamespace: "cd1-ns", cdName: "cd1",
		},
		{
			testName:    "test1",
			cdNamespace: "cd1-ns", cdName: "cd1",
			desiredServices: []kcmv1.Service{
				{Namespace: "A", Name: "a"},
			},
			objects: []client.Object{
				&kcmv1.ServiceSet{
					ObjectMeta: metav1.ObjectMeta{Namespace: "cd1-ns", Name: "cd1"},
					Spec:       kcmv1.ServiceSetSpec{Cluster: "cd1"},
					Status: kcmv1.ServiceSetStatus{
						Services: []kcmv1.ServiceState{
							{Namespace: "A", Name: "a", State: kcmv1.ServiceStateProvisioning},
						},
					},
				},
			},
			expected: []kcmv1.Service{
				{Namespace: "A", Name: "a"},
			},
		},
		{
			testName:    "test2",
			cdNamespace: "cd1-ns", cdName: "cd1",
			desiredServices: []kcmv1.Service{
				{Namespace: "A", Name: "a"},
			},
			objects: []client.Object{
				&kcmv1.ServiceSet{
					ObjectMeta: metav1.ObjectMeta{Namespace: "cd1-ns", Name: "cd1"},
					Spec:       kcmv1.ServiceSetSpec{Cluster: "cd1"},
					Status: kcmv1.ServiceSetStatus{
						Services: []kcmv1.ServiceState{
							{Namespace: "A", Name: "a", State: kcmv1.ServiceStateDeployed},
						},
					},
				},
			},
			expected: []kcmv1.Service{
				{Namespace: "A", Name: "a"},
			},
		},
		{
			testName:    "test3",
			cdNamespace: "cd1-ns", cdName: "cd1",
			desiredServices: []kcmv1.Service{
				{Namespace: "A", Name: "a"},
				{Namespace: "B", Name: "b", DependsOn: []kcmv1.ServiceDependsOn{{Namespace: "A", Name: "a"}}},
			},
			objects: []client.Object{
				&kcmv1.ServiceSet{
					ObjectMeta: metav1.ObjectMeta{Namespace: "cd1-ns", Name: "cd1"},
					Spec:       kcmv1.ServiceSetSpec{Cluster: "cd1"},
					Status: kcmv1.ServiceSetStatus{
						Services: []kcmv1.ServiceState{
							{Namespace: "A", Name: "a", State: kcmv1.ServiceStateProvisioning},
						},
					},
				},
			},
			expected: []kcmv1.Service{
				{Namespace: "A", Name: "a"},
			},
		},
		{
			testName:    "test4",
			cdNamespace: "cd1-ns", cdName: "cd1",
			desiredServices: []kcmv1.Service{
				{Namespace: "A", Name: "a"},
				{Namespace: "B", Name: "b", DependsOn: []kcmv1.ServiceDependsOn{{Namespace: "A", Name: "a"}}},
			},
			objects: []client.Object{
				&kcmv1.ServiceSet{
					ObjectMeta: metav1.ObjectMeta{Namespace: "cd1-ns", Name: "cd1"},
					Spec:       kcmv1.ServiceSetSpec{Cluster: "cd1"},
					Status: kcmv1.ServiceSetStatus{
						Services: []kcmv1.ServiceState{
							{Namespace: "A", Name: "a", State: kcmv1.ServiceStateDeployed},
						},
					},
				},
			},
			expected: []kcmv1.Service{
				{Namespace: "A", Name: "a"},
				{Namespace: "B", Name: "b"},
			},
		},
		// TODO: Add more test cases.
	} {
		t.Run(tc.testName, func(t *testing.T) {
			client := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(tc.objects...).
				WithIndex(&kcmv1.ServiceSet{}, kcmv1.ServiceSetClusterIndexKey, kcmv1.ExtractServiceSetCluster).
				Build()

			filtered, err := FilterServiceDependencies(t.Context(), client, tc.cdNamespace, tc.cdName, tc.desiredServices)
			require.NoError(t, err)
			require.Len(t, tc.expected, len(filtered))
			require.ElementsMatch(t, getRelevantFields(tc.expected), getRelevantFields(filtered))
		})
	}
}
