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
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	helmcontrollerv2 "github.com/fluxcd/helm-controller/api/v2"
	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	fluxconditions "github.com/fluxcd/pkg/runtime/conditions"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	addoncontrollerv1beta1 "github.com/projectsveltos/addon-controller/api/v1beta1"
	sveltoscontrollers "github.com/projectsveltos/addon-controller/controllers"
	libsveltosv1beta1 "github.com/projectsveltos/libsveltos/api/v1beta1"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	corev1 "k8s.io/api/core/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	clusterapiv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	crcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/source"

	kcmv1 "github.com/K0rdent/kcm/api/v1beta1"
	"github.com/K0rdent/kcm/internal/helm"
	"github.com/K0rdent/kcm/internal/metrics"
	"github.com/K0rdent/kcm/internal/record"
	"github.com/K0rdent/kcm/internal/sveltos"
	"github.com/K0rdent/kcm/internal/telemetry"
	"github.com/K0rdent/kcm/internal/utils"
	conditionsutil "github.com/K0rdent/kcm/internal/utils/conditions"
	"github.com/K0rdent/kcm/internal/utils/ratelimit"
	"github.com/K0rdent/kcm/internal/utils/validation"
)

var (
	errClusterNotFound         = errors.New("cluster is not found")
	errClusterTemplateNotFound = errors.New("cluster template is not found")

	errClusterDeploymentSpecUpdated = errors.New("cluster deployment spec updated")
	errIPAMNotReady                 = errors.New("IPAM not ready")
	errInvalidIPAMClaimRef          = errors.New("invalid IPAM claim ref")
)

type helmActor interface {
	DownloadChartFromArtifact(ctx context.Context, artifact *sourcev1.Artifact) (*chart.Chart, error)
	InitializeConfiguration(clusterDeployment *kcmv1.ClusterDeployment, log action.DebugLog) (*action.Configuration, error)
	EnsureReleaseWithValues(ctx context.Context, actionConfig *action.Configuration, hcChart *chart.Chart, clusterDeployment *kcmv1.ClusterDeployment) error
}

// ClusterDeploymentReconciler reconciles a ClusterDeployment object
type ClusterDeploymentReconciler struct {
	Client client.Client
	helmActor
	Config                 *rest.Config
	DynamicClient          *dynamic.DynamicClient
	SystemNamespace        string
	GlobalRegistry         string
	GlobalK0sURL           string
	K0sURLCertSecretName   string // Name of a Secret with K0s Download URL Root CA with ca.crt key
	RegistryCertSecretName string // Name of a Secret with Registry Root CA with ca.crt key

	defaultRequeueTime time.Duration

	IsDisabledValidationWH bool // is webhook disabled set via the controller flags
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *ClusterDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := ctrl.LoggerFrom(ctx)
	l.Info("Reconciling ClusterDeployment")

	clusterDeployment := &kcmv1.ClusterDeployment{}
	if err := r.Client.Get(ctx, req.NamespacedName, clusterDeployment); err != nil {
		if apierrors.IsNotFound(err) {
			l.Info("ClusterDeployment not found, ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}

		l.Error(err, "Failed to get ClusterDeployment")
		return ctrl.Result{}, err
	}

	if !clusterDeployment.DeletionTimestamp.IsZero() {
		l.Info("Deleting ClusterDeployment")
		return r.reconcileDelete(ctx, clusterDeployment)
	}

	management := &kcmv1.Management{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: kcmv1.ManagementName}, management); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get Management: %w", err)
	}
	if !management.DeletionTimestamp.IsZero() {
		l.Info("Management is being deleted, skipping ClusterDeployment reconciliation")
		return ctrl.Result{}, nil
	}

	if clusterDeployment.Status.ObservedGeneration == 0 {
		mgmt := &kcmv1.Management{}
		mgmtRef := client.ObjectKey{Name: kcmv1.ManagementName}
		if err := r.Client.Get(ctx, mgmtRef, mgmt); err != nil {
			l.Error(err, "Failed to get Management object")
			return ctrl.Result{}, err
		}
		if err := telemetry.TrackClusterDeploymentCreate(string(mgmt.UID), string(clusterDeployment.UID), clusterDeployment.Spec.Template, clusterDeployment.Spec.DryRun); err != nil {
			l.Error(err, "Failed to track ClusterDeployment creation")
		}
	}

	return r.reconcileUpdate(ctx, clusterDeployment)
}

func (r *ClusterDeploymentReconciler) reconcileUpdate(ctx context.Context, cd *kcmv1.ClusterDeployment) (_ ctrl.Result, err error) {
	l := ctrl.LoggerFrom(ctx)

	if controllerutil.AddFinalizer(cd, kcmv1.ClusterDeploymentFinalizer) {
		if err := r.Client.Update(ctx, cd); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update clusterDeployment %s/%s: %w", cd.Namespace, cd.Name, err)
		}
		return ctrl.Result{}, nil
	}

	if updated, err := utils.AddKCMComponentLabel(ctx, r.Client, cd); updated || err != nil {
		if err != nil {
			l.Error(err, "adding component label")
		}
		return ctrl.Result{}, err
	}

	clusterTpl := &kcmv1.ClusterTemplate{}
	defer func() {
		err = errors.Join(err, r.updateStatus(ctx, cd, clusterTpl))
	}()

	if err := r.handleCertificateSecrets(ctx, cd); err != nil {
		l.Error(err, "failed to handle certificate secrets")
		return ctrl.Result{}, err
	}

	if err = r.Client.Get(ctx, client.ObjectKey{Name: cd.Spec.Template, Namespace: cd.Namespace}, clusterTpl); err != nil {
		l.Error(err, "failed to get ClusterTemplate")
		err = fmt.Errorf("failed to get ClusterTemplate %s/%s: %w", cd.Namespace, cd.Spec.Template, err)
		if r.setCondition(cd, kcmv1.TemplateReadyCondition, err) {
			r.warnf(cd, "ClusterTemplateError", err.Error())
		}
		if r.IsDisabledValidationWH {
			l.Error(err, "failed to get ClusterTemplate, will not retrigger")
			return ctrl.Result{}, nil // no retrigger
		}
		l.Error(err, "failed to get ClusterTemplate")
		return ctrl.Result{}, err
	}

	ipamEnabled := cd.Spec.IPAMClaim.ClusterIPAMClaimRef != "" || cd.Spec.IPAMClaim.ClusterIPAMClaimSpec != nil
	if ipamEnabled {
		// we need to wait until IPAM is bound before processing ClusterDeployment, otherwise we will
		// create a cluster which does not use allocated addresses.
		ipamErr := r.processClusterIPAM(ctx, cd)
		// in case IPAM is not ready yet, need to requeue cluster deployment
		if errors.Is(ipamErr, errIPAMNotReady) {
			return ctrl.Result{RequeueAfter: r.defaultRequeueTime}, nil
		}
		// in case cluster deployment spec was updated, the object will be requeued,
		// hence no need to requeue here
		if errors.Is(ipamErr, errClusterDeploymentSpecUpdated) {
			return ctrl.Result{}, nil
		}
		// in case cluster deployment object refers IPAM claim which refers different cluster deployment,
		// we need to stop reconciliation until cluster deployment's IPAM definition is	fixed: for instance
		// by adding explicit IPAM configuration which will lead to IPAM claim object creation with proper
		// cluster reference.
		if errors.Is(ipamErr, errInvalidIPAMClaimRef) {
			return ctrl.Result{}, nil
		}
		// in case other errors occurred, return an error
		if ipamErr != nil {
			return ctrl.Result{}, fmt.Errorf("failed to process cluster IPAM: %w", ipamErr)
		}
	}

	clusterRes, clusterErr := r.updateCluster(ctx, cd, clusterTpl)
	servicesRes, servicesErr := r.updateServices(ctx, cd)

	if err = errors.Join(clusterErr, servicesErr); err != nil {
		return ctrl.Result{}, err
	}
	if !clusterRes.IsZero() {
		return clusterRes, nil
	}
	if !servicesRes.IsZero() {
		return servicesRes, nil
	}

	return ctrl.Result{}, nil
}

func (r *ClusterDeploymentReconciler) updateCluster(ctx context.Context, cd *kcmv1.ClusterDeployment, clusterTpl *kcmv1.ClusterTemplate) (ctrl.Result, error) {
	if clusterTpl == nil {
		return ctrl.Result{}, errors.New("cluster template cannot be nil")
	}

	l := ctrl.LoggerFrom(ctx)

	r.initClusterConditions(cd)

	if !clusterTpl.Status.Valid {
		errMsg := fmt.Sprintf("ClusterTemplate %s is not marked as valid", client.ObjectKeyFromObject(clusterTpl))
		if clusterTpl.Status.ValidationError != "" {
			errMsg += ": " + clusterTpl.Status.ValidationError
		}
		err := errors.New(errMsg)
		if r.setCondition(cd, kcmv1.TemplateReadyCondition, err) {
			r.warnf(cd, "InvalidClusterTemplate", errMsg)
		}
		if r.IsDisabledValidationWH {
			l.Error(err, "template is not valid, will not retrigger this error")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	r.setCondition(cd, kcmv1.TemplateReadyCondition, nil)
	// template is ok, propagate data from it
	cd.Status.KubernetesVersion = clusterTpl.Status.KubernetesVersion

	var cred *kcmv1.Credential
	if r.IsDisabledValidationWH {
		l.Info("Validating ClusterTemplate K8s compatibility")
		compErr := validation.ClusterTemplateK8sCompatibility(ctx, r.Client, clusterTpl, cd)
		if compErr != nil {
			compErr = fmt.Errorf("failed to validate ClusterTemplate K8s compatibility: %w", compErr)
		}
		r.setCondition(cd, kcmv1.TemplateReadyCondition, compErr)

		l.Info("Validating Credential")
		var credErr error
		if cred, credErr = validation.ClusterDeployCredential(ctx, r.Client, cd, clusterTpl); credErr != nil {
			credErr = fmt.Errorf("failed to validate Credential: %w", credErr)
		}
		r.setCondition(cd, kcmv1.CredentialReadyCondition, credErr)

		if merr := errors.Join(compErr, credErr); merr != nil {
			l.Error(merr, "failed to validate ClusterDeployment, will not retrigger this error")
			return ctrl.Result{}, nil
		}
	}

	err := r.validateConfig(ctx, cd, clusterTpl)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to validate ClusterDeployment configuration: %w", err)
	}

	if !r.IsDisabledValidationWH {
		cred = new(kcmv1.Credential)
		if err := r.Client.Get(ctx, client.ObjectKey{Name: cd.Spec.Credential, Namespace: cd.Namespace}, cred); err != nil {
			err = fmt.Errorf("failed to get Credential %s/%s: %w", cd.Namespace, cd.Spec.Credential, err)
			if r.setCondition(cd, kcmv1.CredentialReadyCondition, err) {
				r.warnf(cd, "CredentialError", err.Error())
			}
			return ctrl.Result{}, err
		}

		if !cred.Status.Ready {
			if r.setCondition(cd, kcmv1.CredentialReadyCondition, fmt.Errorf("the Credential %s is not ready", client.ObjectKeyFromObject(cred))) {
				r.warnf(cd, "CredentialNotReady", "Credential %s/%s is not ready", cd.Namespace, cd.Spec.Credential)
			}
		} else {
			r.setCondition(cd, kcmv1.CredentialReadyCondition, nil)
		}
	}

	if cd.Spec.DryRun {
		r.eventf(cd, "DryRunEnabled", "DryRun mode is enabled. Remove spec.dryRun to proceed with the deployment")
		return ctrl.Result{}, nil
	}

	if err := r.fillHelmValues(cd, cred); err != nil {
		return ctrl.Result{}, err
	}

	hrReconcileOpts := helm.ReconcileHelmReleaseOpts{
		Values: cd.Spec.Config,
		OwnerReference: &metav1.OwnerReference{
			APIVersion: kcmv1.GroupVersion.String(),
			Kind:       kcmv1.ClusterDeploymentKind,
			Name:       cd.Name,
			UID:        cd.UID,
		},
		ChartRef: clusterTpl.Status.ChartRef,
	}
	if clusterTpl.Spec.Helm.ChartSpec != nil {
		hrReconcileOpts.ReconcileInterval = &clusterTpl.Spec.Helm.ChartSpec.Interval.Duration
	}

	// Now create the CAPI cluster by helm releasing the helm chart associated with the cluster template.
	clusterRef := capiClusterReference(cd)
	hr, operation, err := helm.ReconcileHelmRelease(ctx, r.Client, clusterRef.Name, clusterRef.Namespace, hrReconcileOpts)
	if err != nil {
		err = fmt.Errorf("failed to reconcile HelmRelease: %w", err)
		if r.setCondition(cd, kcmv1.HelmReleaseReadyCondition, err) {
			r.warnf(cd, "HelmReleaseReconcileFailed", err.Error())
		}
		return ctrl.Result{}, err
	}
	if operation == controllerutil.OperationResultCreated {
		r.eventf(cd, "HelmReleaseCreated", "Successfully created HelmRelease %s/%s", clusterRef.Namespace, clusterRef.Name)
	}
	if operation == controllerutil.OperationResultUpdated {
		r.eventf(cd, "HelmReleaseUpdated", "Successfully updated HelmRelease %s/%s", clusterRef.Namespace, clusterRef.Name)
	}

	hrReadyCondition := fluxconditions.Get(hr, fluxmeta.ReadyCondition)
	if hrReadyCondition != nil {
		if apimeta.SetStatusCondition(cd.GetConditions(), metav1.Condition{
			Type:    kcmv1.HelmReleaseReadyCondition,
			Status:  hrReadyCondition.Status,
			Reason:  hrReadyCondition.Reason,
			Message: hrReadyCondition.Message,
		}) {
			r.eventf(cd, "HelmReleaseIsReady", "HelmRelease %s/%s is ready", clusterRef.Namespace, clusterRef.Name)
		}
	}

	requeue, err := r.aggregateConditions(ctx, cd)
	if err != nil {
		if requeue {
			return ctrl.Result{RequeueAfter: r.defaultRequeueTime}, err
		}

		return ctrl.Result{}, err
	}

	if requeue || !fluxconditions.IsReady(hr) {
		return ctrl.Result{RequeueAfter: r.defaultRequeueTime}, nil
	}

	return ctrl.Result{}, nil
}

func (r *ClusterDeploymentReconciler) fillHelmValues(cd *kcmv1.ClusterDeployment, cred *kcmv1.Credential) error {
	if err := cd.AddHelmValues(func(values map[string]any) error {
		values["clusterIdentity"] = map[string]any{
			"apiVersion": cred.Spec.IdentityRef.APIVersion,
			"kind":       cred.Spec.IdentityRef.Kind,
			"name":       cred.Spec.IdentityRef.Name,
			"namespace":  cred.Spec.IdentityRef.Namespace,
		}

		global := map[string]any{
			"registry":           r.GlobalRegistry,
			"k0sURL":             r.GlobalK0sURL,
			"registryCertSecret": r.RegistryCertSecretName,
			"k0sURLCertSecret":   r.K0sURLCertSecretName,
		}
		for _, v := range global {
			if v != "" {
				values["global"] = global
				break
			}
		}

		if _, ok := values["clusterLabels"]; !ok {
			// Use the ManagedCluster's own labels if not defined.
			values["clusterLabels"] = cd.GetObjectMeta().GetLabels()
		}

		return nil
	}); err != nil {
		return fmt.Errorf("failed to add helm values for the ClusterDeployment %s/%s: %w", cd.Namespace, cd.Name, err)
	}

	return nil
}

func (r *ClusterDeploymentReconciler) validateConfig(ctx context.Context, cd *kcmv1.ClusterDeployment, clusterTpl *kcmv1.ClusterTemplate) error {
	helmChartArtifact, err := r.getSourceArtifact(ctx, clusterTpl.Status.ChartRef)
	if err != nil {
		err = fmt.Errorf("failed to get HelmChart Artifact: %w", err)
		if r.setCondition(cd, kcmv1.HelmChartReadyCondition, err) {
			r.warnf(cd, "InvalidSource", err.Error())
		}
		return err
	}

	l := ctrl.LoggerFrom(ctx)
	l.Info("Downloading Helm chart")
	hcChart, err := r.DownloadChartFromArtifact(ctx, helmChartArtifact)
	if err != nil {
		err = fmt.Errorf("failed to download HelmChart from Artifact %s: %w", helmChartArtifact.URL, err)
		if r.setCondition(cd, kcmv1.HelmChartReadyCondition, err) {
			r.warnf(cd, "HelmChartDownloadFailed", err.Error())
		}
		return err
	}

	l.Info("Initializing Helm client")
	actionConfig, err := r.InitializeConfiguration(cd, l.WithName("helm-actor").V(1).Info)
	if err != nil {
		return err
	}

	l.Info("Validating Helm chart with provided values")
	if err := r.EnsureReleaseWithValues(ctx, actionConfig, hcChart, cd); err != nil {
		err = fmt.Errorf("failed to validate template with provided configuration: %w", err)
		if r.setCondition(cd, kcmv1.HelmChartReadyCondition, err) {
			r.warnf(cd, "ValidationError", "Invalid configuration provided: %s", err)
		}
		return err
	}

	r.setCondition(cd, kcmv1.HelmChartReadyCondition, nil)
	return nil
}

func (*ClusterDeploymentReconciler) initClusterConditions(cd *kcmv1.ClusterDeployment) (changed bool) {
	// NOTE: do not put here the PredeclaredSecretsExistCondition since it won't be set if no secrets have been set
	for _, typ := range [5]string{
		kcmv1.CredentialReadyCondition,
		kcmv1.HelmReleaseReadyCondition,
		kcmv1.HelmChartReadyCondition,
		kcmv1.TemplateReadyCondition,
		kcmv1.ReadyCondition,
	} {
		// Skip initialization if the condition already exists.
		// This ensures we don't overwrite an existing condition and can accurately detect actual
		// conditions changes later.
		if apimeta.FindStatusCondition(cd.Status.Conditions, typ) != nil {
			continue
		}
		// Skip setting HelmReleaseReady if in DryRun mode
		if typ == kcmv1.HelmReleaseReadyCondition && cd.Spec.DryRun {
			continue
		}
		if apimeta.SetStatusCondition(&cd.Status.Conditions, metav1.Condition{
			Type:               typ,
			Status:             metav1.ConditionUnknown,
			Reason:             kcmv1.ProgressingReason,
			ObservedGeneration: cd.Generation,
		}) {
			changed = true
		}
	}
	return changed
}

func (r *ClusterDeploymentReconciler) updateSveltosClusterCondition(ctx context.Context, clusterDeployment *kcmv1.ClusterDeployment) (bool, error) {
	sveltosClusters := &libsveltosv1beta1.SveltosClusterList{}

	if err := r.Client.List(ctx, sveltosClusters, &client.ListOptions{
		LabelSelector: labels.SelectorFromSet(map[string]string{kcmv1.FluxHelmChartNameKey: clusterDeployment.Name}),
	}); err != nil {
		return true, fmt.Errorf("failed to get sveltos cluster status: %w", err)
	}

	for _, sveltosCluster := range sveltosClusters.Items {
		sveltosCondition := metav1.Condition{
			Status: metav1.ConditionUnknown,
			Type:   kcmv1.SveltosClusterReadyCondition,
		}

		if sveltosCluster.Status.ConnectionStatus == libsveltosv1beta1.ConnectionHealthy {
			sveltosCondition.Status = metav1.ConditionTrue
			sveltosCondition.Message = "sveltos cluster is healthy"
			sveltosCondition.Reason = kcmv1.SucceededReason
		} else {
			sveltosCondition.Status = metav1.ConditionFalse
			sveltosCondition.Reason = kcmv1.FailedReason
			if sveltosCluster.Status.FailureMessage != nil {
				sveltosCondition.Message = *sveltosCluster.Status.FailureMessage
			}
		}
		apimeta.SetStatusCondition(clusterDeployment.GetConditions(), sveltosCondition)
	}

	return false, nil
}

func (r *ClusterDeploymentReconciler) aggregateConditions(ctx context.Context, cd *kcmv1.ClusterDeployment) (bool, error) {
	var (
		requeue bool
		errs    error
	)
	for _, updateConditions := range []func(context.Context, *kcmv1.ClusterDeployment) (bool, error){
		r.updateSveltosClusterCondition,
		r.aggregateCapiConditions,
	} {
		needRequeue, err := updateConditions(ctx, cd)
		if needRequeue {
			requeue = true
		}
		errs = errors.Join(errs, err)
	}
	return requeue, errs
}

func (r *ClusterDeploymentReconciler) aggregateCapiConditions(ctx context.Context, cd *kcmv1.ClusterDeployment) (requeue bool, _ error) {
	clusters := &clusterapiv1.ClusterList{}
	if err := r.Client.List(ctx, clusters, client.MatchingLabels{kcmv1.FluxHelmChartNameKey: cd.Name}, client.Limit(1)); err != nil {
		return false, fmt.Errorf("failed to list clusters for ClusterDeployment %s: %w", client.ObjectKeyFromObject(cd), err)
	}
	if len(clusters.Items) == 0 {
		return false, nil
	}
	cluster := &clusters.Items[0]

	capiCondition, err := conditionsutil.GetCAPIClusterSummaryCondition(cd, cluster)
	if err != nil {
		return true, fmt.Errorf("failed to get condition summary from Cluster %s: %w", client.ObjectKeyFromObject(cluster), err)
	}

	if apimeta.SetStatusCondition(cd.GetConditions(), *capiCondition) {
		if capiCondition.Status == metav1.ConditionTrue {
			r.eventf(cd, "CAPIClusterIsReady", "Cluster has been provisioned")
			return false, nil
		}
		if cd.DeletionTimestamp.IsZero() {
			r.eventf(cd, "CAPIClusterIsProvisioning", "Cluster is provisioning")
		} else {
			r.eventf(cd, "CAPIClusterIsDeleting", "Cluster is deleting")
		}
	}
	return capiCondition.Status != metav1.ConditionTrue, nil
}

func getProjectTemplateResourceRefs(mc *kcmv1.ClusterDeployment, cred *kcmv1.Credential) []addoncontrollerv1beta1.TemplateResourceRef {
	if !mc.Spec.PropagateCredentials || cred.Spec.IdentityRef == nil {
		return nil
	}

	refs := []addoncontrollerv1beta1.TemplateResourceRef{
		{
			Resource:   *cred.Spec.IdentityRef,
			Identifier: "InfrastructureProviderIdentity",
		},
	}

	if !strings.EqualFold(cred.Spec.IdentityRef.Kind, "Secret") {
		refs = append(refs, addoncontrollerv1beta1.TemplateResourceRef{
			Resource: corev1.ObjectReference{
				APIVersion: "v1",
				Kind:       "Secret",
				Namespace:  cred.Spec.IdentityRef.Namespace,
				Name:       cred.Spec.IdentityRef.Name + "-secret",
			},
			Identifier: "InfrastructureProviderIdentitySecret",
		})
	}

	return refs
}

func getProjectPolicyRefs(mc *kcmv1.ClusterDeployment, cred *kcmv1.Credential) []addoncontrollerv1beta1.PolicyRef {
	if !mc.Spec.PropagateCredentials || cred.Spec.IdentityRef == nil {
		return nil
	}

	return []addoncontrollerv1beta1.PolicyRef{
		{
			Kind:           "ConfigMap",
			Namespace:      cred.Spec.IdentityRef.Namespace,
			Name:           cred.Spec.IdentityRef.Name + "-resource-template",
			DeploymentType: addoncontrollerv1beta1.DeploymentTypeRemote,
		},
	}
}

func (*ClusterDeploymentReconciler) initServicesConditions(cd *kcmv1.ClusterDeployment) (changed bool) {
	for _, typ := range [3]string{kcmv1.SveltosProfileReadyCondition, kcmv1.FetchServicesStatusSuccessCondition, kcmv1.ServicesReferencesValidationCondition} {
		// Skip initialization if the condition already exists.
		// This ensures we don't overwrite an existing condition and can accurately detect actual
		// conditions changes later.
		if apimeta.FindStatusCondition(cd.Status.Conditions, typ) != nil {
			continue
		}
		if apimeta.SetStatusCondition(&cd.Status.Conditions, metav1.Condition{
			Type:               typ,
			Status:             metav1.ConditionUnknown,
			Reason:             kcmv1.ProgressingReason,
			ObservedGeneration: cd.Generation,
		}) {
			changed = true
		}
	}

	return changed
}

func (*ClusterDeploymentReconciler) setCondition(cd *kcmv1.ClusterDeployment, typ string, err error) (changed bool) {
	reason, cstatus, msg := kcmv1.SucceededReason, metav1.ConditionTrue, ""
	if err != nil {
		reason, cstatus, msg = kcmv1.FailedReason, metav1.ConditionFalse, err.Error()
	}

	return apimeta.SetStatusCondition(&cd.Status.Conditions, metav1.Condition{
		Type:               typ,
		Status:             cstatus,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: cd.Generation,
	})
}

// updateServices reconciles services provided in ClusterDeployment.Spec.ServiceSpec.
func (r *ClusterDeploymentReconciler) updateServices(ctx context.Context, cd *kcmv1.ClusterDeployment) (_ ctrl.Result, err error) {
	l := ctrl.LoggerFrom(ctx)
	l.Info("Reconciling Services")

	r.initServicesConditions(cd)

	{
		merr := validation.ClusterDeployCrossNamespaceServicesRefs(ctx, cd) // changes must be done in the spec, which is a new event
		if r.IsDisabledValidationWH {
			merr = errors.Join(merr, validation.ServicesHaveValidTemplates(ctx, r.Client, cd.Spec.ServiceSpec.Services, cd.Namespace))
		}

		if merr != nil {
			// at this point 2/3 conditions will have unknown status, 1/3 (services validation) cond will have failed cond
			if r.setCondition(cd, kcmv1.ServicesReferencesValidationCondition, merr) {
				r.warnf(cd, kcmv1.ServicesReferencesValidationFailedReason, merr.Error())
			}

			l.Error(merr, "failed to validate services, will not retrigger this error")
			return ctrl.Result{}, nil // no reason to reconcile further
		}

		if r.setCondition(cd, kcmv1.ServicesReferencesValidationCondition, nil) {
			r.eventf(cd, kcmv1.ServicesReferencesValidationSucceededReason, "Successfully validated services references")
		}
	}

	// servicesErr is handled separately from err because we do not want
	// to set the condition of SveltosProfileReady type to "False"
	// if there is an error while retrieving status for the services.
	var servicesErr error

	// TODO: should be refactored, unmaintainable; requires to refactor the whole conditions-approach (e.g. init on each event); requires to refactor controllers to be moved to dedicated pkgs
	defer func() {
		if r.setCondition(cd, kcmv1.SveltosProfileReadyCondition, err) {
			if err != nil {
				r.warnf(cd, kcmv1.SveltosProfileNotReadyReason, err.Error())
			} else {
				r.eventf(cd, kcmv1.SveltosProfileReadyCondition, "Successfully reconciled %s/%s Profile", cd.Namespace, cd.Name)
			}
		}

		if r.setCondition(cd, kcmv1.FetchServicesStatusSuccessCondition, servicesErr) {
			if servicesErr != nil {
				r.warnf(cd, kcmv1.FetchServicesStatusFailedReason, servicesErr.Error())
			} else {
				r.eventf(cd, kcmv1.FetchServicesStatusSuccessCondition, "Successfully fetched status of services from Sveltos ClusterSummary")
			}
		}

		if r.IsDisabledValidationWH && apierrors.IsNotFound(err) {
			// non-services NotFound errors relate only to ServiceTemplate
			// if they are gone then nothing to do
			err = nil
		}
		err = errors.Join(err, servicesErr)
	}()

	// we need to validate desired services state against the observed state and available upgrade paths
	if err = validation.ValidateUpgradePaths(cd.Spec.ServiceSpec.Services, cd.Status.ServicesUpgradePaths); err != nil {
		return ctrl.Result{}, err
	}

	helmCharts, err := sveltos.GetHelmCharts(ctx, r.Client, cd.Namespace, cd.Spec.ServiceSpec.Services)
	if err != nil {
		return ctrl.Result{}, err
	}
	kustomizationRefs, err := sveltos.GetKustomizationRefs(ctx, r.Client, cd.Namespace, cd.Spec.ServiceSpec.Services)
	if err != nil {
		return ctrl.Result{}, err
	}
	policyRefs, err := sveltos.GetPolicyRefs(ctx, r.Client, cd.Namespace, cd.Spec.ServiceSpec.Services)
	if err != nil {
		return ctrl.Result{}, err
	}

	cred := new(kcmv1.Credential)
	if err := r.Client.Get(ctx, client.ObjectKey{Name: cd.Spec.Credential, Namespace: cd.Namespace}, cred); err != nil {
		return ctrl.Result{}, err
	}

	if _, err := sveltos.ReconcileProfile(ctx, r.Client, cd.Namespace, cd.Name,
		sveltos.ReconcileProfileOpts{
			OwnerReference: &metav1.OwnerReference{
				APIVersion: kcmv1.GroupVersion.String(),
				Kind:       kcmv1.ClusterDeploymentKind,
				Name:       cd.Name,
				UID:        cd.UID,
			},
			LabelSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					kcmv1.FluxHelmChartNamespaceKey: cd.Namespace,
					kcmv1.FluxHelmChartNameKey:      cd.Name,
				},
			},
			HelmCharts:        helmCharts,
			KustomizationRefs: kustomizationRefs,
			Priority:          cd.Spec.ServiceSpec.Priority,
			StopOnConflict:    cd.Spec.ServiceSpec.StopOnConflict,
			Reload:            cd.Spec.ServiceSpec.Reload,
			TemplateResourceRefs: append(
				getProjectTemplateResourceRefs(cd, cred), cd.Spec.ServiceSpec.TemplateResourceRefs...,
			),
			PolicyRefs:      append(getProjectPolicyRefs(cd, cred), policyRefs...),
			SyncMode:        cd.Spec.ServiceSpec.SyncMode,
			DriftIgnore:     cd.Spec.ServiceSpec.DriftIgnore,
			DriftExclusions: cd.Spec.ServiceSpec.DriftExclusions,
			ContinueOnError: cd.Spec.ServiceSpec.ContinueOnError,
		}); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to reconcile Profile: %w", err)
	}

	metrics.TrackMetricTemplateUsage(ctx, kcmv1.ClusterTemplateKind, cd.Spec.Template, kcmv1.ClusterDeploymentKind, cd.ObjectMeta, true)

	for _, svc := range cd.Spec.ServiceSpec.Services {
		metrics.TrackMetricTemplateUsage(ctx, kcmv1.ServiceTemplateKind, svc.Template, kcmv1.ClusterDeploymentKind, cd.ObjectMeta, true)
	}

	// NOTE:
	// We are returning nil in the return statements whenever servicesErr != nil
	// because we don't want the error content in servicesErr to be assigned to err.
	// The servicesErr var is joined with err in the defer func() so this function
	// will ultimately return the error in servicesErr instead of nil.
	profile := addoncontrollerv1beta1.Profile{}
	profileRef := client.ObjectKey{Name: cd.Name, Namespace: cd.Namespace}
	if servicesErr = r.Client.Get(ctx, profileRef, &profile); servicesErr != nil {
		servicesErr = fmt.Errorf("failed to get Profile %s to fetch status from its associated ClusterSummary: %w", profileRef.String(), servicesErr)
		return ctrl.Result{}, nil
	}

	if len(cd.Spec.ServiceSpec.Services) == 0 {
		cd.Status.Services = nil
		return ctrl.Result{}, nil
	}

	servicesStatus, servicesErr := getServicesStatus(ctx, r.Client, profileRef, profile.Status.MatchingClusterRefs)
	if servicesErr != nil {
		return ctrl.Result{}, nil
	}

	// Running this loop for the sole purpose of creating
	// a kubernetes event for each change in conditions.
	for _, svc := range servicesStatus {
		idx := slices.IndexFunc(cd.Status.Services, func(o kcmv1.ServiceStatus) bool {
			return svc.ClusterNamespace == o.ClusterNamespace && svc.ClusterName == o.ClusterName
		})

		for _, cond := range svc.Conditions {
			if idx > -1 && apimeta.SetStatusCondition(&cd.Status.Services[idx].Conditions, cond) {
				sveltos.CreateEventFromCondition(cd, cd.Generation, client.ObjectKey{Namespace: svc.ClusterNamespace, Name: svc.ClusterName}, &cond)
			}
			// If idx == -1, then a new service was added to the mcs spec so we should create an event in this case.
			sveltos.CreateEventFromCondition(cd, cd.Generation, client.ObjectKey{Namespace: svc.ClusterNamespace, Name: svc.ClusterName}, &cond)
		}
	}

	// We are overwriting conditions so as to be in-sync with the custom status
	// implemented by Sveltos ClusterSummary object. E.g. If a service has been
	// removed, the ClusterSummary status will not show that service, therefore
	// we also want the entry for that service to be removed from conditions.
	cd.Status.Services = servicesStatus
	l.Info("Successfully updated status of services")
	var servicesUpgradePaths []kcmv1.ServiceUpgradePaths
	servicesUpgradePaths, servicesErr = updateServicesUpgradePaths(ctx, r.Client, cd.Spec.ServiceSpec.Services, cd.Namespace)
	cd.Status.ServicesUpgradePaths = servicesUpgradePaths
	return ctrl.Result{}, nil
}

// updateStatus updates the status for the ClusterDeployment object.
func (r *ClusterDeploymentReconciler) updateStatus(ctx context.Context, cd *kcmv1.ClusterDeployment, template *kcmv1.ClusterTemplate) error {
	apimeta.SetStatusCondition(cd.GetConditions(), getServicesReadinessCondition(cd.Status.Services, len(cd.Spec.ServiceSpec.Services)))

	cd.Status.ObservedGeneration = cd.Generation
	cd.Status.Conditions = updateStatusConditions(cd.Status.Conditions)

	if err := r.setAvailableUpgrades(ctx, cd, template); err != nil {
		return errors.New("failed to set available upgrades")
	}

	if err := r.Client.Status().Update(ctx, cd); err != nil {
		return fmt.Errorf("failed to update status for clusterDeployment %s/%s: %w", cd.Namespace, cd.Name, err)
	}

	return nil
}

func (r *ClusterDeploymentReconciler) getSourceArtifact(ctx context.Context, ref *helmcontrollerv2.CrossNamespaceSourceReference) (*sourcev1.Artifact, error) {
	if ref == nil {
		return nil, errors.New("helm chart source is not provided")
	}

	key := client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}
	hc := new(sourcev1.HelmChart)
	if err := r.Client.Get(ctx, key, hc); err != nil {
		return nil, fmt.Errorf("failed to get HelmChart %s: %w", key, err)
	}

	return hc.GetArtifact(), nil
}

func (r *ClusterDeploymentReconciler) reconcileDelete(ctx context.Context, cd *kcmv1.ClusterDeployment) (result ctrl.Result, err error) {
	l := ctrl.LoggerFrom(ctx)

	defer func() {
		if err == nil {
			metrics.TrackMetricTemplateUsage(ctx, kcmv1.ClusterTemplateKind, cd.Spec.Template, kcmv1.ClusterDeploymentKind, cd.ObjectMeta, false)

			for _, svc := range cd.Spec.ServiceSpec.Services {
				metrics.TrackMetricTemplateUsage(ctx, kcmv1.ServiceTemplateKind, svc.Template, kcmv1.ClusterDeploymentKind, cd.ObjectMeta, false)
			}
		}
		err = errors.Join(err, r.updateStatus(ctx, cd, nil))
	}()

	if _, err = r.aggregateCapiConditions(ctx, cd); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to aggregate conditions from CAPI Cluster for ClusterDeployment %s: %w", client.ObjectKeyFromObject(cd), err)
	}

	// Without explicitly deleting the Profile object, we run into a race condition
	// which prevents Sveltos objects from being removed from the management cluster.
	// It is detailed in https://github.com/projectsveltos/addon-controller/issues/732.
	// We may try to remove the explicit call to Delete once a fix for it has been merged.
	// TODO(https://github.com/K0rdent/kcm/issues/526).
	if err := sveltos.DeleteProfile(ctx, r.Client, cd.Namespace, cd.Name); err != nil {
		return ctrl.Result{}, err
	}

	// Verify that any service templates which have been installed are removed prior to deleting the helm release
	// otherwise the k8s control plane will potentially be deleted prior to cleaning up resources
	clusterRef := capiClusterReference(cd)
	summaryName := sveltoscontrollers.GetClusterSummaryName(addoncontrollerv1beta1.ProfileKind, cd.Name, clusterRef.Name, false)
	summary := addoncontrollerv1beta1.ClusterSummary{}
	// clusterRef.Namespace used because Sveltos creates the
	// ClusterSummary in the same namespace as the target cluster.
	summaryRef := client.ObjectKey{Name: summaryName, Namespace: clusterRef.Namespace}
	if err := r.Client.Get(ctx, summaryRef, &summary); client.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get ClusterSummary %s: %w", summaryRef.String(), err)
	}

	for _, hrSummary := range summary.Status.HelmReleaseSummaries {
		l.Info("Prior to deleting the ClusterDeployment, the services deployed via itself need to be removed",
			"releaseName", hrSummary.ReleaseName,
			"releaseNamespace", hrSummary.ReleaseNamespace,
			"status", hrSummary.Status)
	}

	if len(summary.Status.HelmReleaseSummaries) > 0 {
		// Some services still haven't been fully removed so requeue.
		return ctrl.Result{RequeueAfter: r.defaultRequeueTime}, nil
	}

	if err := r.releaseProviderCluster(ctx, cd); err != nil {
		if r.IsDisabledValidationWH && errors.Is(err, errClusterTemplateNotFound) {
			r.setCondition(cd, kcmv1.DeletingCondition, err)
			l.Error(err, "failed to release provider cluster object due to absent ClusterTemplate, will not retrigger")
			// there is not much to do, we cannot release the clusterdeployment without the clustertemplate
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, err
	}

	err = r.Client.Get(ctx, client.ObjectKeyFromObject(cd), &helmcontrollerv2.HelmRelease{})
	if err == nil { // if NO error
		if err := helm.DeleteHelmRelease(ctx, r.Client, cd.Name, cd.Namespace); err != nil {
			r.setCondition(cd, kcmv1.DeletingCondition, err)
			return ctrl.Result{}, err
		}

		l.Info("HelmRelease still exists, retrying")
		return ctrl.Result{RequeueAfter: r.defaultRequeueTime}, nil
	}
	if !apierrors.IsNotFound(err) {
		r.setCondition(cd, kcmv1.DeletingCondition, err)
		return ctrl.Result{}, err
	}
	r.eventf(cd, "HelmReleaseDeleted", "HelmRelease %s has been deleted", client.ObjectKeyFromObject(cd))

	cluster := &metav1.PartialObjectMetadata{}
	cluster.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "cluster.x-k8s.io",
		Version: "v1beta1",
		Kind:    "Cluster",
	})

	err = r.Client.Get(ctx, client.ObjectKeyFromObject(cd), cluster)
	if err == nil { // if NO error
		l.Info("Cluster still exists, retrying", "cluster name", client.ObjectKeyFromObject(cluster))
		return ctrl.Result{RequeueAfter: r.defaultRequeueTime}, nil
	}
	if !apierrors.IsNotFound(err) {
		r.setCondition(cd, kcmv1.DeletingCondition, err)
		l.Error(err, "failed to get Cluster")
		return ctrl.Result{}, err
	}

	r.setCondition(cd, kcmv1.DeletingCondition, nil)
	if controllerutil.RemoveFinalizer(cd, kcmv1.ClusterDeploymentFinalizer) {
		l.Info("Removing Finalizer", "finalizer", kcmv1.ClusterDeploymentFinalizer)
		if err := r.Client.Update(ctx, cd); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update clusterDeployment %s: %w", client.ObjectKeyFromObject(cd), err)
		}
		r.eventf(cd, "SuccessfulDelete", "ClusterDeployment has been deleted")
	}

	l.Info("ClusterDeployment deleted")

	return ctrl.Result{}, nil
}

func (r *ClusterDeploymentReconciler) getProviderGVKs(ctx context.Context, name string) []schema.GroupVersionKind {
	providerInterfaces := &kcmv1.ProviderInterfaceList{}

	if err := r.Client.List(ctx, providerInterfaces,
		client.MatchingFields{kcmv1.ProviderInterfaceInfrastructureIndexKey: name},
		client.Limit(1)); err != nil {
		return nil
	}

	if len(providerInterfaces.Items) == 0 {
		return nil
	}

	gvks := make([]schema.GroupVersionKind, 0, len(providerInterfaces.Items[0].Spec.ClusterGVKs))

	for _, el := range providerInterfaces.Items[0].Spec.ClusterGVKs {
		gvks = append(gvks, schema.GroupVersionKind{
			Group:   el.Group,
			Version: el.Version,
			Kind:    el.Kind,
		})
	}

	return gvks
}

func (r *ClusterDeploymentReconciler) releaseProviderCluster(ctx context.Context, cd *kcmv1.ClusterDeployment) error {
	providers, err := r.getInfraProvidersNames(ctx, cd.Namespace, cd.Spec.Template)
	if err != nil {
		return err
	}

	// Associate the provider with it's GVK
	for _, provider := range providers {
		gvks := r.getProviderGVKs(ctx, provider)
		if len(gvks) == 0 {
			continue
		}

		cluster, err := r.getProviderCluster(ctx, cd.Namespace, cd.Name, gvks...)
		if err != nil {
			if !errors.Is(err, errClusterNotFound) {
				return err
			}
			return nil
		}

		found, err := r.clusterCAPIMachinesExist(ctx, cd.Namespace, cluster.Name)
		if err != nil {
			continue
		}

		if !found {
			finalizersUpdated, err := r.removeClusterFinalizer(ctx, cluster)
			if finalizersUpdated {
				r.eventf(cd, "ClusterDeleted", "Cluster %s has been deleted", client.ObjectKeyFromObject(cd))
			}
			if err != nil {
				return fmt.Errorf("failed to remove finalizer from %s %s: %w", cluster.Kind, client.ObjectKeyFromObject(cluster), err)
			}
		}
	}

	return nil
}

// getInfraProvidersNames returns the list of exposed infrastructure providers with the `infrastructure-` prefix for provided template
func (r *ClusterDeploymentReconciler) getInfraProvidersNames(ctx context.Context, templateNamespace, templateName string) ([]string, error) {
	template := &kcmv1.ClusterTemplate{}
	templateRef := client.ObjectKey{Name: templateName, Namespace: templateNamespace}
	if err := r.Client.Get(ctx, templateRef, template); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "Failed to get ClusterTemplate", "template namespace", templateNamespace, "template name", templateName)
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("failed to get ClusterTemplate %s: %w", templateRef, errClusterTemplateNotFound)
		}
		return nil, err
	}

	ips := make([]string, 0, len(template.Status.Providers))
	for _, v := range template.Status.Providers {
		if strings.HasPrefix(v, kcmv1.InfrastructureProviderPrefix) {
			ips = append(ips, v)
		}
	}

	return ips, nil
}

// getProviderCluster fetches a first provider Cluster from the given list of GVKs.
func (r *ClusterDeploymentReconciler) getProviderCluster(ctx context.Context, namespace, name string, gvks ...schema.GroupVersionKind) (*metav1.PartialObjectMetadata, error) {
	for _, gvk := range gvks {
		itemsList := &metav1.PartialObjectMetadataList{}
		itemsList.SetGroupVersionKind(gvk)
		if err := r.Client.List(ctx, itemsList, client.InNamespace(namespace), client.MatchingLabels{kcmv1.FluxHelmChartNameKey: name}); err != nil {
			return nil, fmt.Errorf("failed to list %s in namespace %s: %w", gvk.Kind, namespace, err)
		}

		if len(itemsList.Items) > 0 {
			return &itemsList.Items[0], nil
		}
	}

	return nil, errClusterNotFound
}

func (r *ClusterDeploymentReconciler) removeClusterFinalizer(ctx context.Context, cluster *metav1.PartialObjectMetadata) (finalizersUpdated bool, err error) {
	originalCluster := *cluster
	if finalizersUpdated = controllerutil.RemoveFinalizer(cluster, kcmv1.BlockingFinalizer); finalizersUpdated {
		ctrl.LoggerFrom(ctx).Info("Allow to stop cluster", "finalizer", kcmv1.BlockingFinalizer)
		if err := r.Client.Patch(ctx, cluster, client.MergeFrom(&originalCluster)); err != nil {
			return false, fmt.Errorf("failed to patch cluster %s/%s: %w", cluster.Namespace, cluster.Name, err)
		}
	}

	return finalizersUpdated, nil
}

func (r *ClusterDeploymentReconciler) clusterCAPIMachinesExist(ctx context.Context, namespace, clusterName string) (bool, error) {
	gvkMachine := schema.GroupVersionKind{
		Group:   "cluster.x-k8s.io",
		Version: "v1beta1",
		Kind:    "Machine",
	}

	itemsList := &metav1.PartialObjectMetadataList{}
	itemsList.SetGroupVersionKind(gvkMachine)
	if err := r.Client.List(ctx, itemsList, client.InNamespace(namespace), client.Limit(1), client.MatchingLabels{clusterapiv1.ClusterNameLabel: clusterName}); err != nil {
		return false, err
	}
	return len(itemsList.Items) != 0, nil
}

func (r *ClusterDeploymentReconciler) setAvailableUpgrades(ctx context.Context, clusterDeployment *kcmv1.ClusterDeployment, clusterTpl *kcmv1.ClusterTemplate) error {
	if clusterTpl == nil {
		return nil
	}

	chains := new(kcmv1.ClusterTemplateChainList)
	if err := r.Client.List(ctx, chains,
		client.InNamespace(clusterTpl.Namespace),
		client.MatchingFields{kcmv1.TemplateChainSupportedTemplatesIndexKey: clusterTpl.Name},
	); err != nil {
		return fmt.Errorf("failed to list ClusterTemplateChains: %w", err)
	}

	availableUpgradesMap := make(map[string]kcmv1.AvailableUpgrade)
	for _, chain := range chains.Items {
		for _, supportedTemplate := range chain.Spec.SupportedTemplates {
			if supportedTemplate.Name == clusterTpl.Name {
				for _, availableUpgrade := range supportedTemplate.AvailableUpgrades {
					availableUpgradesMap[availableUpgrade.Name] = availableUpgrade
				}
			}
		}
	}

	availableUpgrades := make([]string, 0, len(availableUpgradesMap))
	for _, availableUpgrade := range availableUpgradesMap {
		availableUpgrades = append(availableUpgrades, availableUpgrade.Name)
	}

	clusterDeployment.Status.AvailableUpgrades = availableUpgrades
	return nil
}

// templatesValidUpdateSource is a source of update and create events which enqueues ClusterDeployment objects if the referenced ServiceTemplate or ClusterTemplate object gets the valid status.
func (*ClusterDeploymentReconciler) templatesValidUpdateSource(cl client.Client, cache crcache.Cache, obj client.Object) source.TypedSource[ctrl.Request] {
	var isServiceTemplateKind bool // quick kludge to avoid complicated switches
	var indexKey string

	switch obj.(type) {
	case *kcmv1.ServiceTemplate:
		isServiceTemplateKind = true
		indexKey = kcmv1.ClusterDeploymentServiceTemplatesIndexKey
	case *kcmv1.ClusterTemplate:
		indexKey = kcmv1.ClusterDeploymentTemplateIndexKey
	default:
		panic(fmt.Sprintf("unexpected type %T, expected one of [%T, %T]", obj, new(kcmv1.ServiceTemplate), new(kcmv1.ClusterTemplate)))
	}

	return source.TypedKind(cache, obj, handler.TypedEnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []ctrl.Request {
		clds := new(kcmv1.ClusterDeploymentList)
		if err := cl.List(ctx, clds, client.InNamespace(o.GetNamespace()), client.MatchingFields{indexKey: o.GetName()}); err != nil {
			return nil
		}

		resp := make([]ctrl.Request, 0, len(clds.Items))
		for _, v := range clds.Items {
			resp = append(resp, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&v)})
		}

		return resp
	}), predicate.TypedFuncs[client.Object]{
		GenericFunc: func(event.TypedGenericEvent[client.Object]) bool { return false },
		DeleteFunc:  func(event.TypedDeleteEvent[client.Object]) bool { return false },
		UpdateFunc: func(tue event.TypedUpdateEvent[client.Object]) bool {
			// NOTE: might be optimized, probably with go's core types gone (>=go1.25)
			if isServiceTemplateKind {
				sto, ok := tue.ObjectOld.(*kcmv1.ServiceTemplate)
				if !ok {
					return false
				}
				stn, ok := tue.ObjectNew.(*kcmv1.ServiceTemplate)
				if !ok {
					return false
				}
				return stn.Status.Valid && !sto.Status.Valid
			}

			cto, ok := tue.ObjectOld.(*kcmv1.ClusterTemplate)
			if !ok {
				return false
			}
			ctn, ok := tue.ObjectNew.(*kcmv1.ClusterTemplate)
			if !ok {
				return false
			}
			return ctn.Status.Valid && !cto.Status.Valid
		},
	})
}

func (r *ClusterDeploymentReconciler) processClusterIPAM(ctx context.Context, cd *kcmv1.ClusterDeployment) error {
	// a compliment check of input values
	if cd.Spec.IPAMClaim.ClusterIPAMClaimRef == "" && cd.Spec.IPAMClaim.ClusterIPAMClaimSpec == nil {
		return nil
	}

	clusterIpamClaim := kcmv1.ClusterIPAMClaim{}
	// if the ClusterIPAMClaimSpec is not nil we need to create a new ClusterIPAMClaim object
	// or ensure the configuration of the existing ClusterIPAMClaim object. Then we need to
	// update the ClusterIPAMClaimRef in case it does not match the name of the ClusterIPAMClaim object.
	if cd.Spec.IPAMClaim.ClusterIPAMClaimSpec != nil {
		claimName := cd.Name + "-ipam"
		clusterIpamClaim.Name = claimName
		clusterIpamClaim.Namespace = cd.Namespace
		utils.AddOwnerReference(&clusterIpamClaim, cd)

		_, err := ctrl.CreateOrUpdate(ctx, r.Client, &clusterIpamClaim, func() error {
			clusterIpamClaim.Spec = *cd.Spec.IPAMClaim.ClusterIPAMClaimSpec
			return nil
		})
		if err != nil {
			return fmt.Errorf("failed to create or update ClusterIPAMClaim: %w", err)
		}

		if cd.Spec.IPAMClaim.ClusterIPAMClaimRef != clusterIpamClaim.Name {
			cd.Spec.IPAMClaim.ClusterIPAMClaimRef = claimName
			if err := r.Client.Update(ctx, cd); err != nil {
				return fmt.Errorf("failed to update ClusterDeployment: %w", err)
			}
			return errClusterDeploymentSpecUpdated
		}
	} else {
		clusterIpamClaimRef := client.ObjectKey{Name: cd.Spec.IPAMClaim.ClusterIPAMClaimRef, Namespace: cd.Namespace}
		err := r.Client.Get(ctx, clusterIpamClaimRef, &clusterIpamClaim)
		if err != nil {
			return fmt.Errorf("failed to fetch ClusterIPAMClaim: %w", err)
		}
		if clusterIpamClaim.Spec.Cluster != cd.Name {
			return errors.Join(errInvalidIPAMClaimRef, fmt.Errorf(
				"ClusterIPAMClaim.Spec.Cluster %s does not match ClusterDeployment.Name %s", clusterIpamClaim.Spec.Cluster, cd.Name))
		}
	}

	if !clusterIpamClaim.Status.Bound {
		return errIPAMNotReady
	}

	clusterIpamRef := client.ObjectKey{Name: clusterIpamClaim.Spec.ClusterIPAMRef, Namespace: cd.Namespace}
	clusterIpam := kcmv1.ClusterIPAM{}
	if err := r.Client.Get(ctx, clusterIpamRef, &clusterIpam); err != nil {
		return fmt.Errorf("failed to fetch ClusterIPAM: %w", err)
	}

	needsUpdate, err := configNeedsUpdate(cd.Spec.Config, clusterIpam.Status.ProviderData)
	if err != nil {
		return fmt.Errorf("failed to determine whether config needs update: %w", err)
	}
	if needsUpdate {
		if err := cd.AddHelmValues(func(values map[string]any) error {
			values["ipamEnabled"] = true
			for _, v := range clusterIpam.Status.ProviderData {
				values[v.Name] = v
			}
			return nil
		}); err != nil {
			return fmt.Errorf("failed to add IPAM Helm values: %w", err)
		}
		if err := r.Client.Update(ctx, cd); err != nil {
			return fmt.Errorf("failed to update ClusterDeployment: %w", err)
		}
		return errClusterDeploymentSpecUpdated
	}

	return nil
}

func (r *ClusterDeploymentReconciler) handleCertificateSecrets(ctx context.Context, cd *kcmv1.ClusterDeployment) error {
	secretsToHandle := []string{r.K0sURLCertSecretName, r.RegistryCertSecretName}

	l := ctrl.LoggerFrom(ctx).WithName("handle-secrets")

	if _, err := utils.SetPredeclaredSecretsCondition(ctx, r.Client, cd, record.Warnf, r.SystemNamespace, secretsToHandle...); err != nil {
		l.Error(err, "failed to check if given Secrets exist")
		return err
	}

	if cd.Namespace == r.SystemNamespace { // nothing to copy
		return nil
	}

	l.V(1).Info("Copying certificate secrets from the system namespace to the ClusterDeployment namespace")
	for _, secretName := range secretsToHandle {
		if err := utils.CopySecret(ctx, r.Client, client.ObjectKey{Namespace: r.SystemNamespace, Name: secretName}, cd.Namespace); err != nil {
			l.Error(err, "failed to copy Secret for the ClusterDeployment")
			return err
		}
	}

	return nil
}

// capiClusterReference returns the NamespacedName to be
// used for the CAPI cluster created via the clustertemplate.
//
// NOTE: This function isn't strictly needed but created to make
// sure that if there is any change in naming the CAPI cluster,
// then it is reflected across all the places where it is used.
func capiClusterReference(cd *kcmv1.ClusterDeployment) types.NamespacedName {
	return types.NamespacedName{Namespace: cd.Namespace, Name: cd.Name}
}

func configNeedsUpdate(config *apiextv1.JSON, providerData []kcmv1.ClusterIPAMProviderData) (bool, error) {
	// Check if values are already present in the config
	valuesNeedUpdate := false

	// Convert cd.Spec.Config to a map for checking
	var currentValues map[string]any
	if config != nil {
		if err := json.Unmarshal(config.Raw, &currentValues); err != nil {
			return false, fmt.Errorf("failed to unmarshal current config values: %w", err)
		}
	} else {
		currentValues = make(map[string]any)
	}

	// Check if ipamEnabled is already set correctly
	ipamEnabled, ipamEnabledExists := currentValues["ipamEnabled"].(bool)
	if !ipamEnabledExists || !ipamEnabled {
		valuesNeedUpdate = true
	}

	// Check if all provider data values are present
	if !valuesNeedUpdate {
		for _, v := range providerData {
			if _, exists := currentValues[v.Name]; !exists {
				valuesNeedUpdate = true
				break
			}
		}
	}
	return valuesNeedUpdate, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Client = mgr.GetClient()
	r.Config = mgr.GetConfig()

	r.helmActor = helm.NewActor(r.Config, r.Client.RESTMapper())

	r.defaultRequeueTime = 10 * time.Second

	managedController := ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.TypedOptions[ctrl.Request]{
			RateLimiter: ratelimit.DefaultFastSlow(),
		}).
		For(&kcmv1.ClusterDeployment{}).
		Watches(&helmcontrollerv2.HelmRelease{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []ctrl.Request {
				clusterDeploymentRef := client.ObjectKeyFromObject(o)
				if err := r.Client.Get(ctx, clusterDeploymentRef, &kcmv1.ClusterDeployment{}); err != nil {
					return []ctrl.Request{}
				}

				return []ctrl.Request{{NamespacedName: clusterDeploymentRef}}
			}),
		).
		Watches(&kcmv1.ClusterTemplateChain{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []ctrl.Request {
				chain, ok := o.(*kcmv1.ClusterTemplateChain)
				if !ok {
					return nil
				}

				var req []ctrl.Request
				for _, template := range getTemplateNamesManagedByChain(chain) {
					clusterDeployments := &kcmv1.ClusterDeploymentList{}
					err := r.Client.List(ctx, clusterDeployments,
						client.InNamespace(chain.Namespace),
						client.MatchingFields{kcmv1.ClusterDeploymentTemplateIndexKey: template})
					if err != nil {
						return []ctrl.Request{}
					}
					for _, cluster := range clusterDeployments.Items {
						req = append(req, ctrl.Request{
							NamespacedName: client.ObjectKey{
								Namespace: cluster.Namespace,
								Name:      cluster.Name,
							},
						})
					}
				}
				return req
			}),
			builder.WithPredicates(predicate.Funcs{
				UpdateFunc:  func(event.UpdateEvent) bool { return false },
				GenericFunc: func(event.GenericEvent) bool { return false },
			}),
		).
		Watches(&addoncontrollerv1beta1.ClusterSummary{},
			handler.EnqueueRequestsFromMapFunc(requeueSveltosProfileForClusterSummary),
			builder.WithPredicates(predicate.Funcs{
				DeleteFunc:  func(event.DeleteEvent) bool { return false },
				GenericFunc: func(event.GenericEvent) bool { return false },
			}),
		).
		Watches(&kcmv1.Credential{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []ctrl.Request {
				clusterDeployments := &kcmv1.ClusterDeploymentList{}
				err := r.Client.List(ctx, clusterDeployments,
					client.InNamespace(o.GetNamespace()),
					client.MatchingFields{kcmv1.ClusterDeploymentCredentialIndexKey: o.GetName()})
				if err != nil {
					return []ctrl.Request{}
				}

				req := []ctrl.Request{}
				for _, cluster := range clusterDeployments.Items {
					req = append(req, ctrl.Request{
						NamespacedName: client.ObjectKey{
							Namespace: cluster.Namespace,
							Name:      cluster.Name,
						},
					})
				}

				return req
			}),
		)

	if r.IsDisabledValidationWH {
		setupLog := mgr.GetLogger().WithName("clusterdeployment_ctrl_setup")
		managedController.WatchesRawSource(r.templatesValidUpdateSource(mgr.GetClient(), mgr.GetCache(), &kcmv1.ServiceTemplate{}))
		setupLog.Info("Validations are disabled, watcher for ServiceTemplate objects is set")
		managedController.WatchesRawSource(r.templatesValidUpdateSource(mgr.GetClient(), mgr.GetCache(), &kcmv1.ClusterTemplate{}))
		setupLog.Info("Validations are disabled, watcher for ClusterTemplate objects is set")
	}

	return managedController.Complete(r)
}

func (*ClusterDeploymentReconciler) eventf(cd *kcmv1.ClusterDeployment, reason, message string, args ...any) {
	record.Eventf(cd, cd.Generation, reason, message, args...)
}

func (*ClusterDeploymentReconciler) warnf(cd *kcmv1.ClusterDeployment, reason, message string, args ...any) {
	record.Warnf(cd, cd.Generation, reason, message, args...)
}
