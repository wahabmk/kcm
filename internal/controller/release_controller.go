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
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	helmcontrollerv2 "github.com/fluxcd/helm-controller/api/v2"
	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	fluxconditions "github.com/fluxcd/pkg/runtime/conditions"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/storage/driver"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/source"

	kcmv1 "github.com/K0rdent/kcm/api/v1beta1"
	"github.com/K0rdent/kcm/internal/build"
	"github.com/K0rdent/kcm/internal/helm"
	"github.com/K0rdent/kcm/internal/record"
	"github.com/K0rdent/kcm/internal/utils"
	"github.com/K0rdent/kcm/internal/utils/ratelimit"
)

// ReleaseReconciler reconciles a Template object
type ReleaseReconciler struct {
	client.Client

	Config *rest.Config

	KCMTemplatesChartName string
	SystemNamespace       string

	DefaultRegistryConfig helm.DefaultRegistryConfig

	CreateManagement bool
	CreateRelease    bool
	CreateTemplates  bool
}

func (r *ReleaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, err error) {
	l := ctrl.LoggerFrom(ctx).WithValues("controller", "ReleaseController")
	l.Info("Reconciling Release")
	defer l.Info("Release reconcile is finished")

	{
		management := &kcmv1.Management{}
		err := r.Get(ctx, client.ObjectKey{Name: kcmv1.ManagementName}, management)
		if err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("failed to get Management: %w", err)
		}
		if !management.DeletionTimestamp.IsZero() {
			l.Info("Management is being deleted, skipping release reconciliation")
			return ctrl.Result{}, nil
		}
	}

	release := &kcmv1.Release{}
	if req.Name != "" {
		err := r.Get(ctx, req.NamespacedName, release)
		if err != nil {
			if apierrors.IsNotFound(err) {
				l.Info("Release not found, ignoring since object must be deleted")
				return ctrl.Result{}, nil
			}
			l.Error(err, "failed to get Release")
			return ctrl.Result{}, err
		}

		if updated, err := utils.AddKCMComponentLabel(ctx, r.Client, release); updated || err != nil {
			if err != nil {
				l.Error(err, "adding component label")
			}
			return ctrl.Result{}, err
		}

		defer func() {
			release.Status.ObservedGeneration = release.Generation
			for _, condition := range release.Status.Conditions {
				if condition.Status != metav1.ConditionTrue {
					release.Status.Ready = false
				}
			}
			err = errors.Join(err, r.Status().Update(ctx, release))
		}()
	}

	requeue, err := r.reconcileKCMTemplates(ctx, release.Name, release.Spec.Version, release.UID)
	r.updateTemplatesCreatedCondition(release, err)
	if err != nil {
		l.Error(err, "failed to reconcile KCM Templates")
		return ctrl.Result{}, err
	}
	if requeue {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	if release.Name == "" {
		if err := r.ensureManagement(ctx); err != nil {
			l.Error(err, "failed to create Management object")
			r.eventf(release, "ManagementCreationFailed", err.Error())
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}
	err = r.validateProviderTemplates(ctx, release.Name, release.Templates())
	r.updateTemplatesValidCondition(release, err)
	if err != nil {
		l.Error(err, "failed to validate provider templates")
		return ctrl.Result{}, err
	}
	release.Status.Ready = true
	return ctrl.Result{}, nil
}

func (r *ReleaseReconciler) validateProviderTemplates(ctx context.Context, releaseName string, expectedTemplates []string) error {
	providerTemplates := &kcmv1.ProviderTemplateList{}
	if err := r.List(ctx, providerTemplates, client.MatchingFields{kcmv1.OwnerRefIndexKey: releaseName}); err != nil {
		return err
	}
	validTemplates := make(map[string]bool)
	for _, t := range providerTemplates.Items {
		validTemplates[t.Name] = t.Status.ObservedGeneration == t.Generation && t.Status.Valid
	}
	invalidTemplates := []string{}
	for _, t := range expectedTemplates {
		if !validTemplates[t] {
			invalidTemplates = append(invalidTemplates, t)
		}
	}
	if len(invalidTemplates) > 0 {
		return fmt.Errorf("missing or invalid templates: %s", strings.Join(invalidTemplates, ", "))
	}
	return nil
}

func (r *ReleaseReconciler) updateTemplatesValidCondition(release *kcmv1.Release, err error) (changed bool) {
	condition := metav1.Condition{
		Type:               kcmv1.TemplatesValidCondition,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: release.Generation,
		Reason:             kcmv1.SucceededReason,
		Message:            "All templates are valid",
	}
	if err != nil {
		condition.Status = metav1.ConditionFalse
		condition.Message = err.Error()
		condition.Reason = kcmv1.FailedReason
		release.Status.Ready = false
	}

	changed = meta.SetStatusCondition(&release.Status.Conditions, condition)
	if changed && err != nil {
		r.warnf(release, "InvalidProviderTemplates", err.Error())
	}

	return changed
}

func (r *ReleaseReconciler) updateTemplatesCreatedCondition(release *kcmv1.Release, err error) (changed bool) {
	condition := metav1.Condition{
		Type:               kcmv1.TemplatesCreatedCondition,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: release.Generation,
		Reason:             kcmv1.SucceededReason,
		Message:            "All templates have been created",
	}
	if !r.CreateTemplates {
		condition.Message = "Templates creation is disabled"
	}
	if err != nil {
		condition.Status = metav1.ConditionFalse
		condition.Message = err.Error()
		condition.Reason = kcmv1.FailedReason
	}

	changed = meta.SetStatusCondition(&release.Status.Conditions, condition)
	if changed && err != nil {
		r.warnf(release, "TemplatesCreationFailed", err.Error())
	}

	return changed
}

func (r *ReleaseReconciler) ensureManagement(ctx context.Context) error {
	l := ctrl.LoggerFrom(ctx)
	if !r.CreateManagement {
		return nil
	}
	l.Info("Ensuring Management is created")
	mgmtObj := &kcmv1.Management{
		ObjectMeta: metav1.ObjectMeta{
			Name:       kcmv1.ManagementName,
			Finalizers: []string{kcmv1.ManagementFinalizer},
		},
	}
	err := r.Get(ctx, client.ObjectKey{
		Name: kcmv1.ManagementName,
	}, mgmtObj)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get %s Management object: %w", kcmv1.ManagementName, err)
	}
	release, err := r.getCurrentRelease(ctx)
	if err != nil {
		return err
	}
	mgmtObj.Spec.Release = release.Name

	getter := helm.NewMemoryRESTClientGetter(r.Config, r.RESTMapper())
	actionConfig := new(action.Configuration)
	err = actionConfig.Init(getter, r.SystemNamespace, "secret", l.Info)
	if err != nil {
		return err
	}

	kcmConfig := make(chartutil.Values)
	helmRelease, err := actionConfig.Releases.Last("kcm")
	if err != nil {
		if !errors.Is(err, driver.ErrReleaseNotFound) {
			return err
		}
	} else {
		if len(helmRelease.Config) > 0 {
			chartutil.CoalesceTables(kcmConfig, helmRelease.Config)
		}
	}
	rawConfig, err := json.Marshal(kcmConfig)
	if err != nil {
		return err
	}

	mgmtObj.Spec.Providers = release.Providers()
	mgmtObj.Spec.Core = &kcmv1.Core{
		KCM: kcmv1.Component{
			Config: &apiextv1.JSON{
				Raw: rawConfig,
			},
		},
	}
	err = r.Create(ctx, mgmtObj)
	if err != nil {
		return fmt.Errorf("failed to create %s Management object: %w", kcmv1.ManagementName, err)
	}

	l.Info("Successfully created Management object with default configuration")
	return nil
}

func (r *ReleaseReconciler) reconcileKCMTemplates(ctx context.Context, releaseName, releaseVersion string, releaseUID types.UID) (requeue bool, err error) {
	l := ctrl.LoggerFrom(ctx)
	if !r.CreateTemplates {
		l.Info("Templates creation is disabled")
		return false, nil
	}
	if releaseName == "" && !r.CreateRelease {
		l.Info("Initial creation of KCM Release is skipped")
		return false, nil
	}

	initialInstall := releaseName == ""

	ownerRef := &metav1.OwnerReference{
		APIVersion: kcmv1.GroupVersion.String(),
		Kind:       kcmv1.ReleaseKind,
		Name:       releaseName,
		UID:        releaseUID,
	}
	if initialInstall {
		ownerRef = nil

		helmRepositorySecrets := []string{r.DefaultRegistryConfig.CertSecretName, r.DefaultRegistryConfig.CredentialsSecretName}
		exists, missingSecrets, err := utils.CheckAllSecretsExistInNamespace(ctx, r.Client, r.SystemNamespace, helmRepositorySecrets...)
		if err != nil {
			return false, fmt.Errorf("failed to check if Secrets %v exists: %w", helmRepositorySecrets, err)
		}
		if !exists {
			return false, fmt.Errorf("some of the predeclared Secrets (%v) are missing (%v) in the %s namespace", helmRepositorySecrets, missingSecrets, r.SystemNamespace)
		}

		releaseName, err = utils.ReleaseNameFromVersion(build.Version)
		if err != nil {
			return false, fmt.Errorf("failed to get Release name from version %q: %w", build.Version, err)
		}

		releaseVersion = build.Version
		if err := helm.ReconcileHelmRepository(ctx, r.Client, kcmv1.DefaultRepoName, r.SystemNamespace, r.DefaultRegistryConfig.HelmRepositorySpec()); err != nil {
			l.Error(err, "Failed to reconcile default HelmRepository", "namespace", r.SystemNamespace)
			return false, err
		}
	}

	kcmTemplatesName := utils.TemplatesChartFromReleaseName(releaseName)
	helmChart := &sourcev1.HelmChart{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kcmTemplatesName,
			Namespace: r.SystemNamespace,
		},
		TypeMeta: metav1.TypeMeta{
			Kind:       sourcev1.HelmChartKind,
			APIVersion: sourcev1.GroupVersion.String(),
		},
	}

	operation, err := ctrl.CreateOrUpdate(ctx, r.Client, helmChart, func() error {
		if ownerRef != nil {
			helmChart.OwnerReferences = []metav1.OwnerReference{*ownerRef}
		}
		if helmChart.Labels == nil {
			helmChart.Labels = make(map[string]string)
		}
		helmChart.Labels[kcmv1.KCMManagedLabelKey] = kcmv1.KCMManagedLabelValue
		helmChart.Spec.Chart = r.KCMTemplatesChartName
		helmChart.Spec.Version = releaseVersion
		helmChart.Spec.SourceRef = kcmv1.DefaultSourceRef
		helmChart.Spec.Interval = metav1.Duration{Duration: helm.DefaultReconcileInterval}
		return nil
	})
	if err != nil {
		return false, err
	}
	if operation == controllerutil.OperationResultCreated || operation == controllerutil.OperationResultUpdated {
		l.Info("Successfully mutated HelmChart", "HelmChart", client.ObjectKeyFromObject(helmChart), "operation_result", operation)
	}

	opts := helm.ReconcileHelmReleaseOpts{
		ChartRef: &helmcontrollerv2.CrossNamespaceSourceReference{
			Kind:      helmChart.Kind,
			Name:      helmChart.Name,
			Namespace: helmChart.Namespace,
		},
		OwnerReference: ownerRef,
	}

	if initialInstall {
		createReleaseValues := map[string]any{
			"createRelease": true,
		}
		raw, err := json.Marshal(createReleaseValues)
		if err != nil {
			return false, err
		}
		opts.Values = &apiextv1.JSON{Raw: raw}
	}

	hr, operation, err := helm.ReconcileHelmRelease(ctx, r.Client, kcmTemplatesName, r.SystemNamespace, opts)
	if err != nil {
		return false, err
	}
	if operation == controllerutil.OperationResultCreated || operation == controllerutil.OperationResultUpdated {
		l.Info("Successfully mutated HelmRelease", "HelmRelease", client.ObjectKeyFromObject(hr), "operation_result", operation)
	}
	hrReadyCondition := fluxconditions.Get(hr, fluxmeta.ReadyCondition)
	if hrReadyCondition == nil || hrReadyCondition.ObservedGeneration != hr.Generation {
		l.Info("HelmRelease is not ready yet, retrying", "HelmRelease", client.ObjectKeyFromObject(hr))
		return true, nil
	}
	if hrReadyCondition.Status == metav1.ConditionFalse {
		l.Info("HelmRelease is not ready yet", "HelmRelease", client.ObjectKeyFromObject(hr), "message", hrReadyCondition.Message)
		return true, nil
	}
	return false, nil
}

func (r *ReleaseReconciler) getCurrentRelease(ctx context.Context) (*kcmv1.Release, error) {
	releases := &kcmv1.ReleaseList{}
	listOptions := client.ListOptions{
		FieldSelector: fields.SelectorFromSet(fields.Set{kcmv1.ReleaseVersionIndexKey: build.Version}),
	}
	if err := r.List(ctx, releases, &listOptions); err != nil {
		return nil, err
	}
	if len(releases.Items) != 1 {
		return nil, fmt.Errorf("expected 1 Release with version %s, found %d", build.Version, len(releases.Items))
	}
	return &releases.Items[0], nil
}

func (*ReleaseReconciler) eventf(release *kcmv1.Release, reason, message string, args ...any) {
	record.Eventf(release, release.Generation, reason, message, args...)
}

func (*ReleaseReconciler) warnf(release *kcmv1.Release, reason, message string, args ...any) {
	record.Warnf(release, release.Generation, reason, message, args...)
}

// SetupWithManager sets up the controller with the Manager.
func (r *ReleaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	c, err := ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.TypedOptions[ctrl.Request]{
			RateLimiter: ratelimit.DefaultFastSlow(),
		}).
		For(&kcmv1.Release{}, builder.WithPredicates(predicate.Funcs{
			DeleteFunc:  func(event.DeleteEvent) bool { return false },
			GenericFunc: func(event.GenericEvent) bool { return false },
		})).
		Build(r)
	if err != nil {
		return err
	}
	//
	if !r.CreateManagement && !r.CreateRelease {
		return nil
	}
	// There's no Release objects created yet and we need to trigger reconcile
	initChannel := make(chan event.GenericEvent, 1)
	initChannel <- event.GenericEvent{Object: &kcmv1.Release{}}
	return c.Watch(source.Channel(initChannel, &handler.EnqueueRequestForObject{}))
}
