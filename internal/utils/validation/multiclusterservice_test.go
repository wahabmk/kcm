package validation

import (
	"testing"

	kcmv1 "github.com/K0rdent/kcm/api/v1beta1"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestValidateMCSDependency(t *testing.T) {
	for _, tc := range []struct {
		testName    string
		mcs         *kcmv1.MultiClusterService
		mcsList     *kcmv1.MultiClusterServiceList
		expectedErr string
	}{
		{
			testName: "empty",
		},
		{
			testName: "single mcs",
			mcs: &kcmv1.MultiClusterService{
				ObjectMeta: metav1.ObjectMeta{Name: "a"},
			},
		},
		{
			testName: "mcs A->B but B doesn't exist",
			mcs: &kcmv1.MultiClusterService{
				ObjectMeta: metav1.ObjectMeta{Name: "a"},
				Spec:       kcmv1.MultiClusterServiceSpec{DependsOn: []string{"b"}},
			},
			expectedErr: "dependency /b of /a is not defined",
		},
		// TODO: Add more test cases
	} {
		t.Run(tc.testName, func(t *testing.T) {
			if err := validateMCSDependency(tc.mcs, tc.mcsList); err != nil {
				require.EqualError(t, err, tc.expectedErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestValidateMCSDependencyCycle(t *testing.T) {
	for _, tc := range []struct {
		testName string
		mcs      *kcmv1.MultiClusterService
		mcsList  *kcmv1.MultiClusterServiceList
		isErr    bool
	}{
		{
			testName: "empty",
		},
		{
			testName: "single mcs",
			mcs: &kcmv1.MultiClusterService{
				ObjectMeta: metav1.ObjectMeta{Name: "a"},
			},
		},
		{
			testName: "mcs A->B",
			mcs: &kcmv1.MultiClusterService{
				ObjectMeta: metav1.ObjectMeta{Name: "a"},
				Spec:       kcmv1.MultiClusterServiceSpec{DependsOn: []string{"b"}},
			},
			mcsList: &kcmv1.MultiClusterServiceList{
				Items: []kcmv1.MultiClusterService{
					{
						ObjectMeta: metav1.ObjectMeta{Name: "b"},
					},
				},
			},
		},
		{
			testName: "mcs B->A",
			mcs: &kcmv1.MultiClusterService{
				ObjectMeta: metav1.ObjectMeta{Name: "a"},
			},
			mcsList: &kcmv1.MultiClusterServiceList{
				Items: []kcmv1.MultiClusterService{
					{
						ObjectMeta: metav1.ObjectMeta{Name: "b"},
						Spec:       kcmv1.MultiClusterServiceSpec{DependsOn: []string{"a"}},
					},
				},
			},
		},
		{
			testName: "mcs A->A",
			mcs: &kcmv1.MultiClusterService{
				ObjectMeta: metav1.ObjectMeta{Name: "a"},
				Spec:       kcmv1.MultiClusterServiceSpec{DependsOn: []string{"a"}},
			},
			isErr: true,
		},
		{
			testName: "mcs A<->B",
			mcs: &kcmv1.MultiClusterService{
				ObjectMeta: metav1.ObjectMeta{Name: "a"},
				Spec:       kcmv1.MultiClusterServiceSpec{DependsOn: []string{"b"}},
			},
			mcsList: &kcmv1.MultiClusterServiceList{
				Items: []kcmv1.MultiClusterService{
					{
						ObjectMeta: metav1.ObjectMeta{Name: "b"},
						Spec:       kcmv1.MultiClusterServiceSpec{DependsOn: []string{"a"}},
					},
				},
			},
			isErr: true,
		},
		// TODO: Add more tests
	} {
		t.Run(tc.testName, func(t *testing.T) {
			err := validateMCSDependencyCycle(tc.mcs, tc.mcsList)
			if tc.isErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
