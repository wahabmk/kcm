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
	"encoding/json"
	"fmt"
	"time"

	sveltosv1beta1 "github.com/projectsveltos/addon-controller/api/v1beta1"
	libsveltosv1beta1 "github.com/projectsveltos/libsveltos/api/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	hmc "github.com/Mirantis/hmc/api/v1alpha1"
)

const (
	DefaultReconcileInterval = 10 * time.Minute
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
	CreateNamespace  bool
}

func ReconcileClusterProfile(ctx context.Context,
	cl client.Client,
	name string,
	namespace string,
	opts ReconcileClusterProfileOpts,
) (*sveltosv1beta1.ClusterProfile, controllerutil.OperationResult, error) {

	cp := &sveltosv1beta1.ClusterProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
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
					MatchLabels: map[string]string{
						"helm.toolkit.fluxcd.io/name":      name,
						"helm.toolkit.fluxcd.io/namespace": namespace,
					},
				},
			},
		}

		for _, hc := range opts.HelmChartOpts {
			cp.Spec.HelmCharts = append(cp.Spec.HelmCharts, sveltosv1beta1.HelmChart{
				// helm pull oci://hmc-local-registry:5000/charts/ingress-nginx --version 2.0.0 --plain-http
				// This URL can be retrieved from servicetemplate -> helmchart -> helmrepository -> spec.url
				RepositoryURL:    hc.RepositoryURL,
				RepositoryName:   hc.RepositoryName,
				ChartName:        hc.ChartName,
				ChartVersion:     hc.ChartVersion,
				ReleaseName:      hc.ReleaseName,
				ReleaseNamespace: hc.ReleaseNamespace,
				HelmChartAction:  sveltosv1beta1.HelmChartActionInstall,
				Options: &sveltosv1beta1.HelmOptions{
					InstallOptions: sveltosv1beta1.HelmInstallOptions{
						CreateNamespace: hc.CreateNamespace,
					},
				},
				RegistryCredentialsConfig: &sveltosv1beta1.RegistryCredentialsConfig{
					PlainHTTP: true,
				},
				// TLSConfig: &sveltosv1beta1.TLSConfig{
				// 	InsecureSkipTLSVerify: true,
				// },
			})
		}

		fmt.Printf("\n\n******************************************** CClusterProfile *************************************************\n")
		b, _ := json.MarshalIndent(cp, "", "  ")
		fmt.Println(string(b))
		fmt.Printf("***************************************************************************************************************\n\n")

		return nil
	})

	if err != nil {
		return nil, operation, err
	}

	return cp, operation, nil
}

func DeleteClusterProfile(ctx context.Context, cl client.Client, name string, namespace string) error {
	err := cl.Delete(ctx, &sveltosv1beta1.ClusterProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	})
	if client.IgnoreNotFound(err) != nil {
		return err
	}
	return nil
}
