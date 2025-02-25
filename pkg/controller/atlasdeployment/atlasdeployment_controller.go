/*
Copyright 2020 MongoDB.

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

package atlasdeployment

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/mongodb/mongodb-atlas-kubernetes/v2/internal/kube"
	"github.com/mongodb/mongodb-atlas-kubernetes/v2/internal/pointer"
	"github.com/mongodb/mongodb-atlas-kubernetes/v2/internal/translation/deployment"
	"github.com/mongodb/mongodb-atlas-kubernetes/v2/internal/translation/project"
	"github.com/mongodb/mongodb-atlas-kubernetes/v2/pkg/api"
	akov2 "github.com/mongodb/mongodb-atlas-kubernetes/v2/pkg/api/v1"
	"github.com/mongodb/mongodb-atlas-kubernetes/v2/pkg/api/v1/status"
	"github.com/mongodb/mongodb-atlas-kubernetes/v2/pkg/controller/atlas"
	"github.com/mongodb/mongodb-atlas-kubernetes/v2/pkg/controller/connectionsecret"
	"github.com/mongodb/mongodb-atlas-kubernetes/v2/pkg/controller/customresource"
	"github.com/mongodb/mongodb-atlas-kubernetes/v2/pkg/controller/statushandler"
	"github.com/mongodb/mongodb-atlas-kubernetes/v2/pkg/controller/validate"
	"github.com/mongodb/mongodb-atlas-kubernetes/v2/pkg/controller/workflow"
	"github.com/mongodb/mongodb-atlas-kubernetes/v2/pkg/indexer"
)

// AtlasDeploymentReconciler reconciles an AtlasDeployment object
type AtlasDeploymentReconciler struct {
	Client                      client.Client
	Log                         *zap.SugaredLogger
	Scheme                      *runtime.Scheme
	GlobalPredicates            []predicate.Predicate
	EventRecorder               record.EventRecorder
	AtlasProvider               atlas.Provider
	ObjectDeletionProtection    bool
	SubObjectDeletionProtection bool
	independentSyncPeriod       time.Duration

	deploymentService deployment.AtlasDeploymentsService
	projectService    project.ProjectService
}

// +kubebuilder:rbac:groups=atlas.mongodb.com,resources=atlasdeployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=atlas.mongodb.com,resources=atlasdeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=atlas.mongodb.com,namespace=default,resources=atlasdeployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=atlas.mongodb.com,namespace=default,resources=atlasdeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=atlas.mongodb.com,resources=atlasbackupschedules,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=atlas.mongodb.com,resources=atlasbackupschedules/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=atlas.mongodb.com,namespace=default,resources=atlasbackupschedules,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=atlas.mongodb.com,namespace=default,resources=atlasbackupschedules/status,verbs=get;update;patch

// +kubebuilder:rbac:groups=atlas.mongodb.com,resources=atlasbackuppolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=atlas.mongodb.com,resources=atlasbackuppolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=atlas.mongodb.com,namespace=default,resources=atlasbackuppolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=atlas.mongodb.com,namespace=default,resources=atlasbackuppolicies/status,verbs=get;update;patch

// +kubebuilder:rbac:groups=atlas.mongodb.com,resources=atlassearchindexconfigs,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=atlas.mongodb.com,resources=atlassearchindexconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=atlas.mongodb.com,namespace=default,resources=atlassearchindexconfigs,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=atlas.mongodb.com,namespace=default,resources=atlassearchindexconfigs/status,verbs=get;update;patch

// +kubebuilder:rbac:groups="",namespace=default,resources=events,verbs=create;patch

func (r *AtlasDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.With("atlasdeployment", req.NamespacedName)

	atlasDeployment := &akov2.AtlasDeployment{}
	result := customresource.PrepareResource(ctx, r.Client, req, atlasDeployment, log)
	if !result.IsOk() {
		return result.ReconcileResult(), nil
	}

	if shouldSkip := customresource.ReconciliationShouldBeSkipped(atlasDeployment); shouldSkip {
		log.Infow(fmt.Sprintf("-> Skipping AtlasDeployment reconciliation as annotation %s=%s", customresource.ReconciliationPolicyAnnotation, customresource.ReconciliationPolicySkip), "spec", atlasDeployment.Spec)
		if !atlasDeployment.GetDeletionTimestamp().IsZero() {
			err := r.removeDeletionFinalizer(ctx, atlasDeployment)
			if err != nil {
				result = workflow.Terminate(workflow.Internal, err.Error())
				log.Errorw("failed to remove finalizer", "error", err)
				return result.ReconcileResult(), nil
			}
		}
		return workflow.OK().ReconcileResult(), nil
	}

	conditions := akov2.InitCondition(atlasDeployment, api.FalseCondition(api.ReadyType))
	workflowCtx := workflow.NewContext(log, conditions, ctx)
	log.Infow("-> Starting AtlasDeployment reconciliation", "spec", atlasDeployment.Spec, "status", atlasDeployment.Status)
	defer func() {
		statushandler.Update(workflowCtx, r.Client, r.EventRecorder, atlasDeployment)
	}()

	resourceVersionIsValid := customresource.ValidateResourceVersion(workflowCtx, atlasDeployment, r.Log)
	if !resourceVersionIsValid.IsOk() {
		r.Log.Debugf("deployment validation result: %v", resourceVersionIsValid)
		return resourceVersionIsValid.ReconcileResult(), nil
	}

	if !r.AtlasProvider.IsResourceSupported(atlasDeployment) {
		result = workflow.Terminate(workflow.AtlasGovUnsupported, "the AtlasDeployment is not supported by Atlas for government").
			WithoutRetry()
		workflowCtx.SetConditionFromResult(api.DeploymentReadyType, result)
		return result.ReconcileResult(), nil
	}

	var atlasProject *project.Project
	var err error
	if atlasDeployment.Spec.ExternalProjectRef != nil {
		atlasProject, err = r.getProjectFromAtlas(workflowCtx, atlasDeployment)
	} else {
		atlasProject, err = r.getProjectFromKube(workflowCtx, atlasDeployment)
	}
	if err != nil {
		return r.terminate(workflowCtx, workflow.AtlasAPIAccessNotConfigured, err)
	}

	if err = validate.AtlasDeployment(atlasDeployment, r.AtlasProvider.IsCloudGov(), atlasProject.RegionUsageRestrictions); err != nil {
		result = workflow.Terminate(workflow.Internal, err.Error())
		workflowCtx.SetConditionFromResult(api.ValidationSucceeded, result)
		return result.ReconcileResult(), nil
	}
	workflowCtx.SetConditionTrue(api.ValidationSucceeded)

	deploymentInAKO := deployment.NewDeployment(atlasProject.ID, atlasDeployment)
	deploymentInAtlas, err := r.deploymentService.GetDeployment(workflowCtx.Context, atlasProject.ID, atlasDeployment.GetDeploymentName())
	if err != nil {
		return r.terminate(workflowCtx, workflow.Internal, err)
	}

	isServerless := atlasDeployment.IsServerless()
	wasDeleted := !atlasDeployment.GetDeletionTimestamp().IsZero()
	existsInAtlas := deploymentInAtlas != nil

	if existsInAtlas && deploymentInAKO.IsServerless() != deploymentInAtlas.IsServerless() {
		return r.terminate(workflowCtx, workflow.Internal, errors.New("regular deployment cannot be converted into a serverless deployment and vice-versa"))
	}

	switch {
	case existsInAtlas && wasDeleted:
		return r.delete(workflowCtx, deploymentInAKO)
	case !existsInAtlas && wasDeleted:
		return r.unmanage(workflowCtx, atlasDeployment)
	case !wasDeleted && isServerless:
		var serverlessDeployment *deployment.Serverless
		if existsInAtlas {
			serverlessDeployment = deploymentInAtlas.(*deployment.Serverless)
		}
		return r.handleServerlessInstance(workflowCtx, deploymentInAKO.(*deployment.Serverless), serverlessDeployment)
	case !wasDeleted && !isServerless:
		var clusterDeployment *deployment.Cluster
		if existsInAtlas {
			clusterDeployment = deploymentInAtlas.(*deployment.Cluster)
		}
		return r.handleAdvancedDeployment(workflowCtx, deploymentInAKO.(*deployment.Cluster), clusterDeployment)
	}

	return workflow.OK().ReconcileResult(), nil
}

func (r *AtlasDeploymentReconciler) getProjectFromAtlas(ctx *workflow.Context, atlasDeployment *akov2.AtlasDeployment) (*project.Project, error) {
	sdkClient, orgID, err := r.AtlasProvider.SdkClient(
		ctx.Context,
		&client.ObjectKey{Namespace: atlasDeployment.Namespace, Name: atlasDeployment.Credentials().Name},
		r.Log,
	)
	if err != nil {
		return nil, err
	}

	sdkClientSet, _, err := r.AtlasProvider.SdkClientSet(
		ctx.Context,
		&client.ObjectKey{Namespace: atlasDeployment.Namespace, Name: atlasDeployment.Credentials().Name},
		r.Log)
	if err != nil {
		return nil, err
	}

	ctx.SdkClient = sdkClient
	ctx.SdkClientSet = sdkClientSet
	ctx.OrgID = orgID

	r.projectService = project.NewProjectAPIService(sdkClient.ProjectsApi)
	r.deploymentService = deployment.NewAtlasDeployments(sdkClient.ClustersApi, sdkClient.ServerlessInstancesApi, sdkClient.GlobalClustersApi, r.AtlasProvider.IsCloudGov())

	atlasProject, err := r.projectService.GetProject(ctx.Context, atlasDeployment.Spec.ExternalProjectRef.ID)
	if err != nil {
		return nil, err
	}

	// Need to still set old client for component not yet migrated
	ctx.Client, _, err = r.AtlasProvider.Client(
		ctx.Context,
		&client.ObjectKey{Namespace: atlasDeployment.Namespace, Name: atlasDeployment.Credentials().Name},
		r.Log,
	)
	if err != nil {
		return nil, err
	}

	return atlasProject, nil
}

func (r *AtlasDeploymentReconciler) getProjectFromKube(ctx *workflow.Context, atlasDeployment *akov2.AtlasDeployment) (*project.Project, error) {
	atlasProject := &akov2.AtlasProject{}
	if err := r.Client.Get(ctx.Context, atlasDeployment.AtlasProjectObjectKey(), atlasProject); err != nil {
		return nil, err
	}

	credentialsSecret, err := customresource.ComputeSecret(atlasProject, atlasDeployment)
	if err != nil {
		return nil, err
	}

	sdkClient, orgID, err := r.AtlasProvider.SdkClient(ctx.Context, credentialsSecret, r.Log)
	if err != nil {
		return nil, err
	}

	sdkClientSet, _, err := r.AtlasProvider.SdkClientSet(ctx.Context, credentialsSecret, r.Log)
	if err != nil {
		return nil, err
	}

	// Need to still set old client for component not yet migrated
	ctx.Client, _, err = r.AtlasProvider.Client(ctx.Context, credentialsSecret, r.Log)
	if err != nil {
		return nil, err
	}
	ctx.SdkClientSet = sdkClientSet
	ctx.SdkClient = sdkClient
	ctx.OrgID = orgID

	r.deploymentService = deployment.NewAtlasDeployments(sdkClient.ClustersApi, sdkClient.ServerlessInstancesApi, sdkClient.GlobalClustersApi, r.AtlasProvider.IsCloudGov())
	r.projectService = project.NewProjectAPIService(sdkClient.ProjectsApi)

	return project.NewProject(atlasProject, orgID), nil
}

func (r *AtlasDeploymentReconciler) delete(
	ctx *workflow.Context,
	deployment deployment.Deployment, // this must be the original non converted deployment
) (ctrl.Result, error) {
	if err := r.cleanupBindings(ctx.Context, deployment); err != nil {
		return r.terminate(ctx, workflow.Internal, fmt.Errorf("failed to cleanup deployment bindings (backups): %w", err))
	}

	switch {
	case customresource.IsResourcePolicyKeepOrDefault(deployment.GetCustomResource(), r.ObjectDeletionProtection):
		ctx.Log.Info("Not removing Atlas deployment from Atlas as per configuration")
	case customresource.IsResourcePolicyKeep(deployment.GetCustomResource()):
		ctx.Log.Infof("Not removing Atlas deployment from Atlas as the '%s' annotation is set", customresource.ResourcePolicyAnnotation)
	case isTerminationProtectionEnabled(deployment.GetCustomResource()):
		msg := fmt.Sprintf("Termination protection for %s deployment enabled. Deployment in Atlas won't be removed", deployment.GetName())
		ctx.Log.Info(msg)
		r.EventRecorder.Event(deployment.GetCustomResource(), "Warning", "AtlasDeploymentTermination", msg)
	default:
		if err := r.deleteDeploymentFromAtlas(ctx, deployment); err != nil {
			return r.terminate(ctx, workflow.Internal, fmt.Errorf("failed to remove deployment from Atlas: %w", err))
		}
	}

	if err := customresource.ManageFinalizer(ctx.Context, r.Client, deployment.GetCustomResource(), customresource.UnsetFinalizer); err != nil {
		return r.terminate(ctx, workflow.Internal, fmt.Errorf("failed to remove finalizer: %w", err))
	}

	return workflow.OK().ReconcileResult(), nil
}

func (r *AtlasDeploymentReconciler) cleanupBindings(context context.Context, deployment deployment.Deployment) error {
	r.Log.Debug("Cleaning up deployment bindings (backup)")

	return r.garbageCollectBackupResource(context, deployment.GetName())
}

func isTerminationProtectionEnabled(deployment *akov2.AtlasDeployment) bool {
	return (deployment.Spec.DeploymentSpec != nil &&
		deployment.Spec.DeploymentSpec.TerminationProtectionEnabled) || (deployment.Spec.ServerlessSpec != nil &&
		deployment.Spec.ServerlessSpec.TerminationProtectionEnabled)
}

func (r *AtlasDeploymentReconciler) deleteDeploymentFromAtlas(ctx *workflow.Context, deployment deployment.Deployment) error {
	ctx.Log.Infow("-> Starting AtlasDeployment deletion", "spec", deployment)

	err := r.deleteConnectionStrings(ctx, deployment)
	if err != nil {
		return err
	}

	err = r.deploymentService.DeleteDeployment(ctx.Context, deployment)
	if err != nil {
		ctx.Log.Errorw("Cannot delete Atlas deployment", "error", err)
		return err
	}

	return nil
}

func (r *AtlasDeploymentReconciler) deleteConnectionStrings(ctx *workflow.Context, deployment deployment.Deployment) error {
	// We always remove the connection secrets even if the deployment is not removed from Atlas
	secrets, err := connectionsecret.ListByDeploymentName(ctx.Context, r.Client, "", deployment.GetProjectID(), deployment.GetName())
	if err != nil {
		return fmt.Errorf("failed to find connection secrets for the user: %w", err)
	}

	for i := range secrets {
		if err := r.Client.Delete(ctx.Context, &secrets[i]); err != nil {
			if k8serrors.IsNotFound(err) {
				continue
			}
			ctx.Log.Errorw("Failed to delete secret", "secretName", secrets[i].Name, "error", err)
		}
	}

	return nil
}

func (r *AtlasDeploymentReconciler) removeDeletionFinalizer(context context.Context, deployment *akov2.AtlasDeployment) error {
	err := r.Client.Get(context, kube.ObjectKeyFromObject(deployment), deployment)
	if err != nil {
		return fmt.Errorf("cannot get AtlasDeployment while adding finalizer: %w", err)
	}

	customresource.UnsetFinalizer(deployment, customresource.FinalizerLabel)
	if err = r.Client.Update(context, deployment); err != nil {
		return fmt.Errorf("failed to remove deletion finalizer from %s: %w", deployment.GetDeploymentName(), err)
	}
	return nil
}

type transitionFn func(reason workflow.ConditionReason) (ctrl.Result, error)

func (r *AtlasDeploymentReconciler) transitionFromLegacy(ctx *workflow.Context, projectID string, atlasDeployment *akov2.AtlasDeployment, err error) transitionFn {
	return func(reason workflow.ConditionReason) (ctrl.Result, error) {
		if err != nil {
			return r.terminate(ctx, reason, err)
		}

		deploymentInAtlas, err := r.deploymentService.GetDeployment(ctx.Context, projectID, atlasDeployment.GetDeploymentName())
		if err != nil {
			return r.terminate(ctx, workflow.Internal, err)
		}

		return r.inProgress(ctx, atlasDeployment, deploymentInAtlas, workflow.DeploymentUpdating, "deployment is updating")
	}
}

func (r *AtlasDeploymentReconciler) transitionFromResult(ctx *workflow.Context, projectID string, atlasDeployment *akov2.AtlasDeployment, result workflow.Result) transitionFn {
	if result.IsInProgress() {
		return func(reason workflow.ConditionReason) (ctrl.Result, error) {
			deploymentInAtlas, err := r.deploymentService.GetDeployment(ctx.Context, projectID, atlasDeployment.GetDeploymentName())
			if err != nil {
				return r.terminate(ctx, workflow.Internal, err)
			}

			return r.inProgress(ctx, atlasDeployment, deploymentInAtlas, workflow.DeploymentUpdating, "deployment is updating")
		}
	}

	if !result.IsOk() {
		return func(reason workflow.ConditionReason) (ctrl.Result, error) {
			return r.terminate(ctx, reason, errors.New(result.GetMessage()))
		}
	}

	return nil
}

func (r *AtlasDeploymentReconciler) terminate(ctx *workflow.Context, errorCondition workflow.ConditionReason, err error) (ctrl.Result, error) {
	r.Log.Error(err)
	terminated := workflow.Terminate(errorCondition, err.Error())
	ctx.SetConditionFromResult(api.DeploymentReadyType, terminated)

	return terminated.ReconcileResult(), nil
}

func (r *AtlasDeploymentReconciler) inProgress(ctx *workflow.Context, atlasDeployment *akov2.AtlasDeployment, deploymentInAtlas deployment.Deployment, reason workflow.ConditionReason, msg string) (ctrl.Result, error) {
	if err := customresource.ManageFinalizer(ctx.Context, r.Client, atlasDeployment, customresource.SetFinalizer); err != nil {
		return r.terminate(ctx, workflow.AtlasFinalizerNotSet, err)
	}

	result := workflow.InProgress(reason, msg)
	ctx.SetConditionFromResult(api.DeploymentReadyType, result).
		EnsureStatusOption(status.AtlasDeploymentStateNameOption(deploymentInAtlas.GetState())).
		EnsureStatusOption(status.AtlasDeploymentReplicaSet(deploymentInAtlas.GetReplicaSet())).
		EnsureStatusOption(status.AtlasDeploymentMongoDBVersionOption(deploymentInAtlas.GetMongoDBVersion()))

	return result.ReconcileResult(), nil
}

func (r *AtlasDeploymentReconciler) ready(ctx *workflow.Context, atlasDeployment *akov2.AtlasDeployment, deploymentInAtlas deployment.Deployment) (ctrl.Result, error) {
	if err := customresource.ManageFinalizer(ctx.Context, r.Client, atlasDeployment, customresource.SetFinalizer); err != nil {
		return r.terminate(ctx, workflow.AtlasFinalizerNotSet, err)
	}

	ctx.SetConditionTrue(api.DeploymentReadyType).
		SetConditionTrue(api.ReadyType).
		EnsureStatusOption(status.AtlasDeploymentStateNameOption(deploymentInAtlas.GetState())).
		EnsureStatusOption(status.AtlasDeploymentReplicaSet(deploymentInAtlas.GetReplicaSet())).
		EnsureStatusOption(status.AtlasDeploymentMongoDBVersionOption(deploymentInAtlas.GetMongoDBVersion())).
		EnsureStatusOption(status.AtlasDeploymentConnectionStringsOption(deploymentInAtlas.GetConnection()))

	if atlasDeployment.Spec.ExternalProjectRef != nil {
		return workflow.Requeue(r.independentSyncPeriod).ReconcileResult(), nil
	}

	return workflow.OK().ReconcileResult(), nil
}

func (r *AtlasDeploymentReconciler) unmanage(ctx *workflow.Context, atlasDeployment *akov2.AtlasDeployment) (ctrl.Result, error) {
	err := r.removeDeletionFinalizer(ctx.Context, atlasDeployment)
	if err != nil {
		return r.terminate(ctx, workflow.AtlasFinalizerNotRemoved, err)
	}

	return workflow.OK().ReconcileResult(), nil
}

func (r *AtlasDeploymentReconciler) SetupWithManager(mgr ctrl.Manager, skipNameValidation bool) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("AtlasDeployment").
		For(&akov2.AtlasDeployment{}, builder.WithPredicates(r.GlobalPredicates...)).
		Watches(
			&akov2.AtlasBackupSchedule{},
			handler.EnqueueRequestsFromMapFunc(r.findDeploymentsForBackupSchedule),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Watches(
			&akov2.AtlasBackupPolicy{},
			handler.EnqueueRequestsFromMapFunc(r.findDeploymentsForBackupPolicy),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Watches(
			&akov2.AtlasSearchIndexConfig{},
			handler.EnqueueRequestsFromMapFunc(r.findDeploymentsForSearchIndexConfig),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.deploymentsForCredentialMapFunc()),
			builder.WithPredicates(predicate.ResourceVersionChangedPredicate{}),
		).
		WithOptions(controller.TypedOptions[reconcile.Request]{SkipNameValidation: pointer.MakePtr(skipNameValidation)}).
		Complete(r)
}

func NewAtlasDeploymentReconciler(
	mgr manager.Manager,
	predicates []predicate.Predicate,
	atlasProvider atlas.Provider,
	deletionProtection bool,
	independentSyncPeriod time.Duration,
	logger *zap.Logger,
) *AtlasDeploymentReconciler {
	suggaredLogger := logger.Named("controllers").Named("AtlasDeployment").Sugar()

	return &AtlasDeploymentReconciler{
		Scheme:                   mgr.GetScheme(),
		Client:                   mgr.GetClient(),
		EventRecorder:            mgr.GetEventRecorderFor("AtlasDeployment"),
		GlobalPredicates:         predicates,
		Log:                      suggaredLogger,
		AtlasProvider:            atlasProvider,
		ObjectDeletionProtection: deletionProtection,
		independentSyncPeriod:    independentSyncPeriod,
	}
}

func (r *AtlasDeploymentReconciler) findDeploymentsForBackupPolicy(ctx context.Context, obj client.Object) []reconcile.Request {
	backupPolicy, ok := obj.(*akov2.AtlasBackupPolicy)
	if !ok {
		r.Log.Warnf("watching AtlasBackupPolicy but got %T", obj)
		return nil
	}

	backupSchedules := &akov2.AtlasBackupScheduleList{}
	listOps := &client.ListOptions{
		FieldSelector: fields.OneTermEqualSelector(
			indexer.AtlasBackupScheduleByBackupPolicyIndex,
			client.ObjectKeyFromObject(backupPolicy).String(),
		),
	}
	err := r.Client.List(ctx, backupSchedules, listOps)
	if err != nil {
		r.Log.Errorf("failed to list Atlas backup schedules: %e", err)
		return []reconcile.Request{}
	}

	deploymentMap := make(map[string]struct{}, len(backupSchedules.Items))
	deployments := make([]reconcile.Request, 0, len(backupSchedules.Items))
	for i := range backupSchedules.Items {
		deploymentKeys := r.findDeploymentsForBackupSchedule(ctx, &backupSchedules.Items[i])
		for j := range deploymentKeys {
			key := deploymentKeys[j].String()
			if _, found := deploymentMap[key]; !found {
				deployments = append(deployments, deploymentKeys[j])
				deploymentMap[key] = struct{}{}
			}
		}
	}

	return deployments
}

func (r *AtlasDeploymentReconciler) findDeploymentsForBackupSchedule(ctx context.Context, obj client.Object) []reconcile.Request {
	backupSchedule, ok := obj.(*akov2.AtlasBackupSchedule)
	if !ok {
		r.Log.Warnf("watching AtlasBackupSchedule but got %T", obj)
		return nil
	}

	deployments := &akov2.AtlasDeploymentList{}
	listOps := &client.ListOptions{
		FieldSelector: fields.OneTermEqualSelector(
			indexer.AtlasDeploymentByBackupScheduleIndex,
			client.ObjectKeyFromObject(backupSchedule).String(),
		),
	}
	err := r.Client.List(ctx, deployments, listOps)
	if err != nil {
		r.Log.Errorf("failed to list Atlas deployments: %e", err)
		return []reconcile.Request{}
	}

	requests := make([]reconcile.Request, 0, len(deployments.Items))
	for i := range deployments.Items {
		item := deployments.Items[i]
		requests = append(
			requests,
			reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      item.Name,
					Namespace: item.Namespace,
				},
			},
		)
	}

	return requests
}

func (r *AtlasDeploymentReconciler) findDeploymentsForSearchIndexConfig(ctx context.Context, obj client.Object) []reconcile.Request {
	searchIndexConfig, ok := obj.(*akov2.AtlasSearchIndexConfig)
	if !ok {
		r.Log.Warnf("watching AtlasSearchIndexConfig but got %T", obj)
		return nil
	}

	deployments := &akov2.AtlasDeploymentList{}
	listOps := &client.ListOptions{
		FieldSelector: fields.OneTermEqualSelector(
			indexer.AtlasDeploymentBySearchIndexIndex,
			client.ObjectKeyFromObject(searchIndexConfig).String(),
		),
	}
	err := r.Client.List(ctx, deployments, listOps)
	if err != nil {
		r.Log.Errorf("failed to list Atlas search index configs: %e", err)
		return []reconcile.Request{}
	}

	requests := make([]reconcile.Request, 0, len(deployments.Items))
	for i := range deployments.Items {
		item := deployments.Items[i]
		requests = append(
			requests,
			reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      item.Name,
					Namespace: item.Namespace,
				},
			},
		)
	}

	return requests
}

func (r *AtlasDeploymentReconciler) deploymentsForCredentialMapFunc() handler.MapFunc {
	return indexer.CredentialsIndexMapperFunc(
		indexer.AtlasDeploymentCredentialsIndex,
		func() *akov2.AtlasDeploymentList { return &akov2.AtlasDeploymentList{} },
		indexer.DeploymentRequests,
		r.Client,
		r.Log,
	)
}
