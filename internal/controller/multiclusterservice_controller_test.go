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

package controller

import (
	"context"
	"time"

	hcv2 "github.com/fluxcd/helm-controller/api/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	hmc "github.com/Mirantis/hmc/api/v1alpha1"
	"github.com/Mirantis/hmc/internal/utils"
)

var _ = Describe("MultiClusterService Controller", func() {
	Context("When reconciling a resource", func() {
		const (
			multiClusterServiceName = "test-multiclusterservice"
			serviceTemplateName     = "test-service-0-1-0"
			serviceReleaseName      = "test-service"
		)

		ctx := context.Background()

		multiClusterServiceRef := types.NamespacedName{
			Name: multiClusterServiceName,
		}
		serviceTemplateRef := types.NamespacedName{
			Namespace: utils.DefaultSystemNamespace,
			Name:      serviceTemplateName,
		}

		multiClusterService := &hmc.MultiClusterService{}
		namespace := &corev1.Namespace{}
		serviceTemplate := &hmc.ServiceTemplate{}

		BeforeEach(func() {
			By("creating hmc-system namespace")
			err := k8sClient.Get(ctx, types.NamespacedName{Name: utils.DefaultSystemNamespace}, namespace)
			if err != nil && errors.IsNotFound(err) {
				namespace = &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name: utils.DefaultSystemNamespace,
					},
				}
				Expect(k8sClient.Create(ctx, namespace)).To(Succeed())
			}

			By("creating the custom resource for the Kind Template")
			err = k8sClient.Get(ctx, serviceTemplateRef, serviceTemplate)
			if err != nil && errors.IsNotFound(err) {
				serviceTemplate = &hmc.ServiceTemplate{
					ObjectMeta: metav1.ObjectMeta{
						Name:      serviceTemplateName,
						Namespace: utils.DefaultSystemNamespace,
					},
					Spec: hmc.ServiceTemplateSpec{
						TemplateSpecCommon: hmc.TemplateSpecCommon{
							Helm: hmc.HelmSpec{
								ChartRef: &hcv2.CrossNamespaceSourceReference{
									Kind:      "HelmChart",
									Name:      "ref-test",
									Namespace: utils.DefaultSystemNamespace,
								},
							},
						},
					},
				}
			}
			Expect(k8sClient.Create(ctx, serviceTemplate)).To(Succeed())

			By("creating the custom resource for the Kind MultiClusterService")
			err = k8sClient.Get(ctx, multiClusterServiceRef, multiClusterService)
			if err != nil && errors.IsNotFound(err) {
				multiClusterService = &hmc.MultiClusterService{
					ObjectMeta: metav1.ObjectMeta{
						Name: multiClusterServiceName,
					},
					Spec: hmc.MultiClusterServiceSpec{
						Services: []hmc.ServiceSpec{
							{
								Template: serviceTemplateName,
								Name:     serviceReleaseName,
							},
						},
					},
				}
				Expect(k8sClient.Create(ctx, multiClusterService)).To(Succeed())
			}
		})

		AfterEach(func() {
			By("Cleanup")

			reconciler := &MultiClusterServiceReconciler{
				Client: k8sClient,
			}

			Expect(k8sClient.Delete(ctx, multiClusterService)).To(Succeed())
			// Running reconcile to remove the finalizer and delete the MultiClusterService
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: multiClusterServiceRef})
			Expect(err).NotTo(HaveOccurred())

			Eventually(k8sClient.Get(ctx, multiClusterServiceRef, multiClusterService), 1*time.Minute, 5*time.Second).Should(HaveOccurred())
			Expect(k8sClient.Delete(ctx, serviceTemplate)).To(Succeed())
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			reconciler := &MultiClusterServiceReconciler{
				Client: k8sClient,
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: multiClusterServiceRef,
			})
			Expect(err).NotTo(HaveOccurred())
			// TODO(user): Add more specific assertions depending on your controller's reconciliation logic.
			// Example: If you expect a certain status condition after reconciliation, verify it here.
		})
	})
})
