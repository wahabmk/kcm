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

package sveltos

import (
	"context"
	"fmt"

	sveltosv1beta1 "github.com/projectsveltos/addon-controller/api/v1beta1"
	libsveltosv1beta1 "github.com/projectsveltos/libsveltos/api/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	hmc "github.com/Mirantis/hmc/api/v1alpha1"
)

type ReconcileClusterProfileOpts struct {
	OwnerReference *metav1.OwnerReference
	HelmChartOpts  []HelmChartOpts
}

type HelmChartOpts struct {
	RepositoryURL    string
	RepositoryName   string
	ChartName        string
	ChartVersion     string
	ReleaseName      string
	ReleaseNamespace string
	Values           string
	CreateNamespace  bool
	PlainHTTP        bool
	Insecure         bool
}

// ClusterProfileName returns a cluster-wide unique name for ClusterProfile object.
func ClusterProfileName(namespace string, name string) string {
	return fmt.Sprintf("%s-%s", namespace, name)
}

// ReconcileClusterProfile reconciles a Sveltos ClusterProfile object.
func ReconcileClusterProfile(ctx context.Context,
	cl client.Client,
	namespace string,
	name string,
	matchLabels map[string]string,
	opts ReconcileClusterProfileOpts,
) (*sveltosv1beta1.ClusterProfile, controllerutil.OperationResult, error) {

	cp := &sveltosv1beta1.ClusterProfile{
		ObjectMeta: metav1.ObjectMeta{
			// ClusterProfile has cluster scope so not setting namespace.
			Name: ClusterProfileName(namespace, name),
		},
	}

	operation, err := ctrl.CreateOrUpdate(ctx, cl, cp, func() error {
		if cp.Labels == nil {
			cp.Labels = make(map[string]string)
		}

		cp.Labels[hmc.HMCManagedLabelKey] = hmc.HMCManagedLabelValue
		if opts.OwnerReference != nil {
			cp.OwnerReferences = []metav1.OwnerReference{*opts.OwnerReference}
		}

		cp.Spec = sveltosv1beta1.Spec{
			ClusterSelector: libsveltosv1beta1.Selector{
				LabelSelector: metav1.LabelSelector{
					MatchLabels: matchLabels,
				},
			},
		}

		for _, hc := range opts.HelmChartOpts {
			cp.Spec.HelmCharts = append(cp.Spec.HelmCharts, sveltosv1beta1.HelmChart{
				RepositoryURL:    hc.RepositoryURL,
				RepositoryName:   hc.RepositoryName,
				ChartName:        hc.ChartName,
				ChartVersion:     hc.ChartVersion,
				ReleaseName:      hc.ReleaseName,
				ReleaseNamespace: hc.ReleaseNamespace,
				Values:           hc.Values,
				HelmChartAction:  sveltosv1beta1.HelmChartActionInstall,
				Options: &sveltosv1beta1.HelmOptions{
					InstallOptions: sveltosv1beta1.HelmInstallOptions{
						CreateNamespace: hc.CreateNamespace,
					},
				},
				RegistryCredentialsConfig: &sveltosv1beta1.RegistryCredentialsConfig{
					PlainHTTP:             hc.PlainHTTP,
					InsecureSkipTLSVerify: hc.Insecure,
				},
			})
		}
		return nil
	})
	if err != nil {
		return nil, operation, err
	}

	return cp, operation, nil
}

// DeleteClusterProfile issues delete on ClusterProfile object.
func DeleteClusterProfile(ctx context.Context, cl client.Client, namespace string, name string) error {
	err := cl.Delete(ctx, &sveltosv1beta1.ClusterProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name: ClusterProfileName(namespace, name),
		},
	})

	if client.IgnoreNotFound(err) != nil {
		return err
	}

	return nil
}
