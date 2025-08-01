/*
Copyright (C) 2022-2025 ApeCloud Co., Ltd

This file is part of KubeBlocks project

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

package restore

import (
	"context"
	"fmt"
	"sort"
	"time"

	vsv1 "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	dpv1alpha1 "github.com/apecloud/kubeblocks/apis/dataprotection/v1alpha1"
	"github.com/apecloud/kubeblocks/pkg/constant"
	"github.com/apecloud/kubeblocks/pkg/controller/instanceset"
	intctrlutil "github.com/apecloud/kubeblocks/pkg/controllerutil"
	dptypes "github.com/apecloud/kubeblocks/pkg/dataprotection/types"
	"github.com/apecloud/kubeblocks/pkg/dataprotection/utils"
	"github.com/apecloud/kubeblocks/pkg/dataprotection/utils/boolptr"
	viper "github.com/apecloud/kubeblocks/pkg/viperx"
)

const (
	restoreManagerContainerName = "restore-manager"
)

type BackupActionSet struct {
	Backup *dpv1alpha1.Backup
	// set it when the backup relies on incremental backups, such as Incremental backup
	AncestorIncrementalBackups []*dpv1alpha1.Backup
	// set it when the backup relies on a base backup, such as Continuous backup
	BaseBackup        *dpv1alpha1.Backup
	ActionSet         *dpv1alpha1.ActionSet
	UseVolumeSnapshot bool
}

type RestoreManager struct {
	OriginalRestore       *dpv1alpha1.Restore
	Restore               *dpv1alpha1.Restore
	PrepareDataBackupSets []BackupActionSet
	PostReadyBackupSets   []BackupActionSet
	Schema                *runtime.Scheme
	Recorder              record.EventRecorder
	Client                client.Client
	WorkerServiceAccount  string
}

func NewRestoreManager(restore *dpv1alpha1.Restore, recorder record.EventRecorder, schema *runtime.Scheme, client client.Client) *RestoreManager {
	return &RestoreManager{
		OriginalRestore:       restore.DeepCopy(),
		Restore:               restore,
		PrepareDataBackupSets: []BackupActionSet{},
		PostReadyBackupSets:   []BackupActionSet{},
		Schema:                schema,
		Recorder:              recorder,
		Client:                client,
	}
}

// GetBackupActionSetByNamespaced gets the BackupActionSet by name and namespace of backup.
func (r *RestoreManager) GetBackupActionSetByNamespaced(reqCtx intctrlutil.RequestCtx,
	cli client.Client,
	backupName,
	namespace string) (*BackupActionSet, error) {
	backup := &dpv1alpha1.Backup{}
	if err := cli.Get(reqCtx.Ctx, types.NamespacedName{Namespace: namespace, Name: backupName}, backup); err != nil {
		if apierrors.IsNotFound(err) {
			err = intctrlutil.NewFatalError(err.Error())
		}
		return nil, err
	}
	backupMethod := backup.Status.BackupMethod
	if backupMethod == nil {
		return nil, intctrlutil.NewFatalError(fmt.Sprintf(`status.backupMethod of backup "%s" is empty`, backupName))
	}
	useVolumeSnapshot := backupMethod.SnapshotVolumes != nil && *backupMethod.SnapshotVolumes
	actionSet, err := utils.GetActionSetByName(reqCtx, cli, backup.Status.BackupMethod.ActionSetName)
	if err != nil {
		return nil, err
	}
	return &BackupActionSet{Backup: backup, ActionSet: actionSet, UseVolumeSnapshot: useVolumeSnapshot}, nil
}

// BuildDifferentialBackupActionSets builds the backupActionSets for specified incremental backup.
func (r *RestoreManager) BuildDifferentialBackupActionSets(reqCtx intctrlutil.RequestCtx, cli client.Client, sourceBackupSet BackupActionSet) error {
	parentBackupSet, err := r.GetBackupActionSetByNamespaced(reqCtx, cli, sourceBackupSet.Backup.Spec.ParentBackupName, sourceBackupSet.Backup.Namespace)
	if err != nil || parentBackupSet == nil {
		return err
	}
	r.SetBackupSets(*parentBackupSet, sourceBackupSet)
	return nil
}

// BuildIncrementalBackupActionSet builds the backupActionSet for specified incremental backup.
func (r *RestoreManager) BuildIncrementalBackupActionSet(reqCtx intctrlutil.RequestCtx, cli client.Client, sourceBackupSet BackupActionSet) error {
	childBackupSet := &sourceBackupSet
	backupMap := map[string]struct{}{}
	for childBackupSet.ActionSet != nil && childBackupSet.ActionSet.Spec.BackupType == dpv1alpha1.BackupTypeIncremental {
		// record the traversed backups
		backupMap[childBackupSet.Backup.Name] = struct{}{}
		// get the parent BackupActionSet for incremental.
		backupSet, err := r.GetBackupActionSetByNamespaced(reqCtx, cli, childBackupSet.Backup.Status.ParentBackupName, childBackupSet.Backup.Namespace)
		if err != nil || backupSet == nil {
			return intctrlutil.NewFatalError(fmt.Sprintf(`fails to get parent backup "%s" of incremental backup "%s"`,
				childBackupSet.Backup.Status.ParentBackupName, childBackupSet.Backup.Name))
		}
		if _, ok := backupMap[backupSet.Backup.Name]; ok {
			return intctrlutil.NewFatalError(fmt.Sprintf(`backup "%s" relies on child backup "%s"`,
				childBackupSet.Backup.Name, backupSet.Backup.Name))
		}
		if err := ValidateParentBackupSet(backupSet, childBackupSet); err != nil {
			return intctrlutil.NewFatalError(fmt.Sprintf(`fails to validate parent backup "%s" and child backup "%s": %v`,
				backupSet.Backup.Name, childBackupSet.Backup.Name, err))
		}
		if backupSet.ActionSet != nil && backupSet.ActionSet.Spec.BackupType == dpv1alpha1.BackupTypeIncremental {
			sourceBackupSet.AncestorIncrementalBackups = append([]*dpv1alpha1.Backup{backupSet.Backup}, sourceBackupSet.AncestorIncrementalBackups...)
		} else {
			sourceBackupSet.BaseBackup = backupSet.Backup
		}
		childBackupSet = backupSet
	}
	r.SetBackupSets(sourceBackupSet)
	return nil
}

func (r *RestoreManager) BuildContinuousRestoreManager(reqCtx intctrlutil.RequestCtx, cli client.Client, continuousBackupSet BackupActionSet) error {
	restoreTime, _ := time.Parse(time.RFC3339, r.Restore.Spec.RestoreTime)
	continuousBackup := continuousBackupSet.Backup
	checkRestoreTime := func() error {
		startTime := continuousBackup.GetStartTime()
		stopTime := continuousBackup.GetEndTime()
		if startTime.IsZero() || stopTime.IsZero() {
			return intctrlutil.NewFatalError(fmt.Sprintf(`startTimeStamp or completeTimeStamp of backup "%s" is empty`, continuousBackup.Name))
		}
		if restoreTime.Before(startTime.Time) || restoreTime.After(stopTime.Time) {
			return intctrlutil.NewFatalError(fmt.Sprintf(`restore time out of the range for backup "%s"`, continuousBackup.Name))
		}
		return nil
	}
	// check if the restore time is valid.
	if err := checkRestoreTime(); err != nil {
		return err
	}

	if continuousBackupSet.ActionSet.Spec.Restore != nil {
		if baseBackupRequired := continuousBackupSet.ActionSet.Spec.Restore.BaseBackupRequired; boolptr.IsSetToFalse(baseBackupRequired) {
			r.SetBackupSets(continuousBackupSet)
			return nil
		}
	}

	baseBackupSet, err := r.getBaseBackupActionSetForContinuous(reqCtx, cli, continuousBackup, metav1.NewTime(restoreTime))
	if err != nil || baseBackupSet == nil {
		return err
	}

	skipBaseBackupRestoreInPitr := false
	if continuousBackupSet.ActionSet.Annotations != nil {
		if continuousBackupSet.ActionSet.Annotations[constant.SkipBaseBackupRestoreInPitrAnnotationKey] == "true" {
			skipBaseBackupRestoreInPitr = true
		}
	}

	// set base backup
	continuousBackupSet.BaseBackup = baseBackupSet.Backup
	if baseBackupSet.ActionSet != nil && baseBackupSet.ActionSet.Spec.BackupType == dpv1alpha1.BackupTypeIncremental {
		if skipBaseBackupRestoreInPitr {
			return intctrlutil.NewFatalError("unify incremental and continuous restore job is not supported")
		}
		if err = r.BuildIncrementalBackupActionSet(reqCtx, cli, *baseBackupSet); err != nil {
			return err
		}
		r.SetBackupSets(continuousBackupSet)
	} else {
		if skipBaseBackupRestoreInPitr {
			r.Recorder.Event(r.Restore, corev1.EventTypeNormal, "SkipBaseBackupRestoreInPitr", "base backup restore skipped")
			r.SetBackupSets(continuousBackupSet)
		} else {
			r.SetBackupSets(*baseBackupSet, continuousBackupSet)
		}
	}
	return nil
}

// getBaseBackupActionSetForContinuous gets full or incremental backup and actionSet for continuous.
func (r *RestoreManager) getBaseBackupActionSetForContinuous(reqCtx intctrlutil.RequestCtx, cli client.Client, continuousBackup *dpv1alpha1.Backup, restoreTime metav1.Time) (*BackupActionSet, error) {
	notFoundLatestBackup := func() (*BackupActionSet, error) {
		return nil, intctrlutil.NewFatalError(fmt.Sprintf(`can not found latest full or incremental backup based on backupPolicy "%s" and specified restoreTime "%s"`,
			continuousBackup.Spec.BackupPolicyName, restoreTime))
	}
	if continuousBackup.GetStartTime().IsZero() {
		return notFoundLatestBackup()
	}
	// 1. list completed backups
	// full backups
	fullBackupItems, err := r.listCompletedBackups(reqCtx, cli, continuousBackup, dpv1alpha1.BackupTypeFull)
	if err != nil {
		return nil, err
	}
	// incremental backups
	incrementalBackupItems, err := r.listCompletedBackups(reqCtx, cli, continuousBackup, dpv1alpha1.BackupTypeIncremental)
	if err != nil {
		return nil, err
	}
	backupItems := []dpv1alpha1.Backup{}
	backupItems = append(backupItems, fullBackupItems...)
	backupItems = append(backupItems, incrementalBackupItems...)
	// sort by completed time in descending order
	sort.Slice(backupItems, func(i, j int) bool {
		i, j = j, i
		return utils.CompareWithBackupStopTime(backupItems[i], backupItems[j])
	})

	// 2. get the latest backup object
	var latestBackup *dpv1alpha1.Backup
	for _, item := range backupItems {
		backupStopTime := item.GetEndTime()
		// latest backup rules:
		// 1. Full or Incremental backup's stopTime must after Continuous backup's startTime.
		//    Even if the seconds are the same, the data may not be continuous.
		// 2. RestoreTime should after the Full or Incremental backup's stopTime.
		if backupStopTime != nil &&
			!restoreTime.Before(backupStopTime) &&
			!backupStopTime.Before(continuousBackup.GetStartTime()) {
			latestBackup = &item
			break
		}
	}
	if latestBackup == nil {
		return notFoundLatestBackup()
	}
	// 3. get the action set
	var actionSetName string
	if latestBackup.Status.BackupMethod != nil {
		actionSetName = latestBackup.Status.BackupMethod.ActionSetName
	}
	actionSet, err := utils.GetActionSetByName(reqCtx, cli, actionSetName)
	if err != nil {
		return nil, err
	}
	return &BackupActionSet{Backup: latestBackup, ActionSet: actionSet}, nil
}

func (r *RestoreManager) listCompletedBackups(reqCtx intctrlutil.RequestCtx, cli client.Client, continuousBackup *dpv1alpha1.Backup, backupType dpv1alpha1.BackupType) ([]dpv1alpha1.Backup, error) {
	matchingLabels := map[string]string{
		dptypes.BackupTypeLabelKey: string(backupType),
	}
	if clusterUID := continuousBackup.Labels[dptypes.ClusterUIDLabelKey]; clusterUID != "" {
		matchingLabels[dptypes.ClusterUIDLabelKey] = clusterUID
	}
	if instance := continuousBackup.Labels[constant.AppInstanceLabelKey]; instance != "" {
		matchingLabels[constant.AppInstanceLabelKey] = instance
	}
	if compName := continuousBackup.Labels[constant.KBAppComponentLabelKey]; compName != "" {
		matchingLabels[constant.KBAppComponentLabelKey] = compName
	}
	if len(matchingLabels) == 1 {
		// if only backupType label exists, need to match based on whether it is the same policy.
		matchingLabels[dptypes.BackupPolicyLabelKey] = continuousBackup.Spec.BackupPolicyName
	}
	backups := dpv1alpha1.BackupList{}
	if err := cli.List(reqCtx.Ctx, &backups,
		client.InNamespace(continuousBackup.Namespace),
		client.MatchingLabels(matchingLabels),
	); err != nil {
		return nil, err
	}
	backupItems := []dpv1alpha1.Backup{}
	for _, b := range backups.Items {
		if b.Status.Phase == dpv1alpha1.BackupPhaseCompleted {
			backupItems = append(backupItems, b)
		}
	}
	return backupItems, nil
}

func (r *RestoreManager) SetBackupSets(backupSets ...BackupActionSet) {
	for i := range backupSets {
		if backupSets[i].UseVolumeSnapshot {
			r.PrepareDataBackupSets = append(r.PrepareDataBackupSets, backupSets[i])
			continue
		}
		if backupSets[i].ActionSet == nil || backupSets[i].ActionSet.Spec.Restore == nil {
			continue
		}
		if backupSets[i].ActionSet.Spec.Restore.PrepareData != nil {
			r.PrepareDataBackupSets = append(r.PrepareDataBackupSets, backupSets[i])
		}

		if len(backupSets[i].ActionSet.Spec.Restore.PostReady) > 0 {
			r.PostReadyBackupSets = append(r.PostReadyBackupSets, backupSets[i])
		}
	}
}

// AnalysisRestoreActionsWithBackup analysis the restore actions progress group by backup.
// check if the restore jobs are completed or failed or processing.
func (r *RestoreManager) AnalysisRestoreActionsWithBackup(stage dpv1alpha1.RestoreStage, backupName string, actionName string) (bool, bool) {
	var (
		restoreActionCount  int
		finishedActionCount int
		existFailedAction   bool
	)
	restoreActions := r.Restore.Status.Actions.PostReady
	if stage == dpv1alpha1.PrepareData {
		restoreActions = r.Restore.Status.Actions.PrepareData
		// if the stage is prepareData, actionCount keeps up with pvc count.
		restoreActionCount = GetRestoreActionsCountForPrepareData(r.Restore.Spec.PrepareDataConfig)
	}
	for i := range restoreActions {
		if restoreActions[i].BackupName != backupName || restoreActions[i].Name != actionName {
			continue
		}
		// if the stage is PostReady, actionCount keeps up with actions
		if stage == dpv1alpha1.PostReady {
			restoreActionCount += 1
		}
		switch restoreActions[i].Status {
		case dpv1alpha1.RestoreActionFailed:
			finishedActionCount += 1
			existFailedAction = true
		case dpv1alpha1.RestoreActionCompleted:
			finishedActionCount += 1
		}
	}

	allActionsFinished := restoreActionCount > 0 && finishedActionCount == restoreActionCount
	return allActionsFinished, existFailedAction
}

func addItsManagingLabels(claim *dpv1alpha1.RestoreVolumeClaim, index int) {
	clusterName := claim.Labels[constant.AppInstanceLabelKey]
	compName := claim.Labels[constant.KBAppComponentLabelKey]
	if clusterName == "" || compName == "" {
		return
	}
	if claim.Labels == nil {
		claim.Labels = make(map[string]string)
	}

	compObjName := constant.GenerateWorkloadNamePattern(clusterName, compName)
	itsMatchLabels := instanceset.GetMatchLabels(compObjName)
	intctrlutil.MergeMetadataMapInplace(itsMatchLabels, &claim.Labels)

	if claim.Labels[constant.KBAppPodNameLabelKey] == "" {
		templateName, exist := claim.Labels[constant.KBAppInstanceTemplateLabelKey]
		var podName string
		if exist {
			podName = fmt.Sprintf("%s-%s-%d", compObjName, templateName, index)
		} else {
			podName = fmt.Sprintf("%s-%d", compObjName, index)
		}
		claim.Labels[constant.KBAppPodNameLabelKey] = podName
	}
}

func (r *RestoreManager) RestorePVCFromSnapshot(reqCtx intctrlutil.RequestCtx, cli client.Client, backupSet BackupActionSet, target *dpv1alpha1.BackupStatusTarget) error {
	prepareDataConfig := r.Restore.Spec.PrepareDataConfig
	if prepareDataConfig == nil {
		return nil
	}
	createPVCWithSnapshot := func(claim dpv1alpha1.RestoreVolumeClaim) error {
		if claim.VolumeSource == "" {
			return intctrlutil.NewFatalError(fmt.Sprintf(`claim "%s"" volumeSource can not be empty if the backup uses volume snapshot`, claim.Name))
		}
		// TODO:  will be removed in 0.10.0, compatibility handling for version 0.8.
		volumeSnapshotName := utils.GetOldBackupVolumeSnapshotName(backupSet.Backup.Name, claim.VolumeSource)
		vsCli := utils.NewCompatClient(cli)
		if exist, err := intctrlutil.CheckResourceExists(reqCtx.Ctx, vsCli,
			types.NamespacedName{Namespace: backupSet.Backup.Namespace, Name: volumeSnapshotName},
			&vsv1.VolumeSnapshot{}); err != nil {
			return err
		} else if !exist {
			sourceTargetPodName, err := GetSourcePodNameFromTarget(target, prepareDataConfig.RequiredPolicyForAllPodSelection, 0)
			if err != nil {
				return err
			}
			if target.PodSelector.Strategy == dpv1alpha1.PodSelectionStrategyAny || sourceTargetPodName != "" {
				snapshotGroup := GetVolumeSnapshotsBySourcePod(backupSet.Backup, target, sourceTargetPodName)
				if snapshotGroup == nil {
					message := fmt.Sprintf(`can not found the volumeSnapshot in status.actions, sourceTargetPod is "%s"`, sourceTargetPodName)
					return intctrlutil.NewFatalError(message)
				}
				volumeSnapshotName = snapshotGroup[claim.VolumeSource]
			}
		}
		if volumeSnapshotName != "" {
			// get volumeSnapshot by backup and volumeSource.
			claim.VolumeClaimSpec.DataSource = &corev1.TypedLocalObjectReference{
				Name:     volumeSnapshotName,
				Kind:     constant.VolumeSnapshotKind,
				APIGroup: &VolumeSnapshotGroup,
			}
		}
		return r.createPVCIfNotExist(reqCtx, cli, claim.ObjectMeta, claim.VolumeClaimSpec)
	}
	for i := range prepareDataConfig.RestoreVolumeClaims {
		if err := createPVCWithSnapshot(prepareDataConfig.RestoreVolumeClaims[i]); err != nil {
			return err
		}
	}
	claimTemplate := prepareDataConfig.RestoreVolumeClaimsTemplate
	if claimTemplate != nil {
		restoreJobReplicas := GetRestoreActionsCountForPrepareData(prepareDataConfig)
		for i := 0; i < restoreJobReplicas; i++ {
			//  create pvc from claims template, build volumes and volumeMounts
			for _, claim := range prepareDataConfig.RestoreVolumeClaimsTemplate.Templates {
				index := i + int(claimTemplate.StartingIndex)
				claim.Name = fmt.Sprintf("%s-%d", claim.Name, index)
				// HACK: add InstanceSet related labels to the PVC,
				// so that it can be managed by InstanceSet
				addItsManagingLabels(&claim, index)
				if err := createPVCWithSnapshot(claim); err != nil {
					return err
				}
			}
		}
	}
	// NOTE: do not to record status action for restoring from snapshot. it is not defined in ActionSet.
	return nil
}

func (r *RestoreManager) prepareBackupRepo(reqCtx intctrlutil.RequestCtx, cli client.Client, backupSet BackupActionSet) (*dpv1alpha1.BackupRepo, error) {
	if backupSet.Backup.Status.BackupRepoName != "" {
		backupRepo := &dpv1alpha1.BackupRepo{}
		err := cli.Get(reqCtx.Ctx, client.ObjectKey{Name: backupSet.Backup.Status.BackupRepoName}, backupRepo)
		if err != nil {
			if apierrors.IsNotFound(err) {
				err = intctrlutil.NewFatalError(err.Error())
			}
			return nil, err
		}
		return backupRepo, nil
	}
	return nil, nil
}

// BuildPrepareDataJobs builds the restore jobs for prepare pvc's data, and will create the target pvcs if not exist.
func (r *RestoreManager) BuildPrepareDataJobs(reqCtx intctrlutil.RequestCtx, cli client.Client, backupSet BackupActionSet, target *dpv1alpha1.BackupStatusTarget, actionName string) ([]*batchv1.Job, error) {
	prepareDataConfig := r.Restore.Spec.PrepareDataConfig
	if prepareDataConfig == nil {
		return nil, nil
	}
	if !backupSet.ActionSet.HasPrepareDataStage() {
		return nil, nil
	}
	backupRepo, err := r.prepareBackupRepo(reqCtx, cli, backupSet)
	if err != nil {
		return nil, err
	}
	jobBuilder := newRestoreJobBuilder(r.Restore, backupSet, backupRepo, dpv1alpha1.PrepareData).
		setImage(backupSet.ActionSet.Spec.Restore.PrepareData.Image).
		setCommand(backupSet.ActionSet.Spec.Restore.PrepareData.Command).
		setServiceAccount(r.WorkerServiceAccount).
		attachBackupRepo()

	createPVCIfNotExistsAndBuildVolume := func(claim dpv1alpha1.RestoreVolumeClaim, identifier string) (*corev1.Volume, *corev1.VolumeMount, error) {
		if err := r.createPVCIfNotExist(reqCtx, cli, claim.ObjectMeta, claim.VolumeClaimSpec); err != nil {
			return nil, nil, err
		}
		return jobBuilder.buildPVCVolumeAndMount(claim.VolumeConfig, claim.Name, identifier)
	}
	for _, claim := range prepareDataConfig.RestoreVolumeClaims {
		// if only restore VolumeClaims, the sourceTargetPod must be consistent for each volumeClaims.
		// otherwise the restored data will be inconsistent.
		// create pvc from volumeClaims, set volume and volumeMount to jobBuilder
		volume, volumeMount, err := createPVCIfNotExistsAndBuildVolume(claim, "dp-claim")
		if err != nil {
			return nil, err
		}
		jobBuilder.addToCommonVolumesAndMounts(volume, volumeMount)
	}

	var (
		restoreJobs        []*batchv1.Job
		restoreJobReplicas = GetRestoreActionsCountForPrepareData(prepareDataConfig)
		claimsTemplate     = prepareDataConfig.RestoreVolumeClaimsTemplate
	)

	if prepareDataConfig.IsSerialPolicy() {
		// obtain the PVC serial number that needs to be restored
		currentOrder := 1
		prepareActions := r.Restore.Status.Actions.PrepareData
		for i := range prepareActions {
			if prepareActions[i].BackupName != backupSet.Backup.Name || prepareActions[i].Name != actionName {
				continue
			}
			if prepareActions[i].Status == dpv1alpha1.RestoreActionCompleted && currentOrder < restoreJobReplicas {
				currentOrder += 1
				if prepareDataConfig.IsSerialPolicy() {
					// if the restore policy is Serial, should delete the completed job to release the pvc.
					if err := deleteRestoreJob(reqCtx, cli, prepareActions[i].ObjectKey, r.Restore.Namespace); err != nil {
						return nil, err
					}
				}
			}
		}
		restoreJobReplicas = currentOrder
	}
	// build restore job to prepare pvc's data
	for i := 0; i < restoreJobReplicas; i++ {
		// reset specific volumes and volumeMounts
		jobBuilder.resetSpecificVolumesAndMounts()
		// reset specific labels as addLabel does not override existing labels
		jobBuilder.resetSpecificLabels()
		if claimsTemplate != nil {
			//  create pvc from claims template, build volumes and volumeMounts
			for _, c := range claimsTemplate.Templates {
				// deepcopy to avoid modify the original object
				claim := *c.DeepCopy()
				index := i + int(claimsTemplate.StartingIndex)
				claim.Name = fmt.Sprintf("%s-%d", claim.Name, index)
				// HACK: add InstanceSet related labels to the PVC,
				// so that it can be managed by InstanceSet
				addItsManagingLabels(&claim, index)
				volume, volumeMount, err := createPVCIfNotExistsAndBuildVolume(claim, "dp-claim-tpl")
				if err != nil {
					return nil, err
				}
				for k, v := range claim.Labels {
					jobBuilder.addLabel(k, v)
				}
				jobBuilder.addToSpecificVolumesAndMounts(volume, volumeMount)
			}
		}
		sourceTargetPodName, err := GetSourcePodNameFromTarget(target, prepareDataConfig.RequiredPolicyForAllPodSelection, i)
		if err != nil {
			return nil, err
		}
		if target.PodSelector.Strategy == dpv1alpha1.PodSelectionStrategyAll && sourceTargetPodName == "" {
			// no need to recover the volume when the pod selection policy is 'All' and sourceTargetPodName is not found.
			continue
		}
		// build job and append
		job := jobBuilder.setJobName(jobBuilder.builderRestoreJobName(i)).addCommonEnv(sourceTargetPodName).build()
		if prepareDataConfig.IsSerialPolicy() &&
			restoreJobHasCompleted(r.Restore.Status.Actions.PrepareData, job.Name) {
			// if the job has completed and the restore policy is Serial, continue
			continue
		}
		restoreJobs = append(restoreJobs, job)
	}
	return restoreJobs, nil
}

func (r *RestoreManager) BuildVolumePopulateJob(
	reqCtx intctrlutil.RequestCtx,
	cli client.Client,
	backupSet BackupActionSet,
	target *dpv1alpha1.BackupStatusTarget,
	populatePVC *corev1.PersistentVolumeClaim,
	index int) (*batchv1.Job, error) {
	prepareDataConfig := r.Restore.Spec.PrepareDataConfig
	if prepareDataConfig == nil || prepareDataConfig.DataSourceRef == nil {
		return nil, nil
	}
	if !backupSet.ActionSet.HasPrepareDataStage() {
		return nil, nil
	}
	backupRepo, err := r.prepareBackupRepo(reqCtx, cli, backupSet)
	if err != nil {
		return nil, err
	}
	sourceTargetPodName, err := GetSourcePodNameFromTarget(target, prepareDataConfig.RequiredPolicyForAllPodSelection, 0)
	if err != nil {
		return nil, err
	}
	jobBuilder := newRestoreJobBuilder(r.Restore, backupSet, backupRepo, dpv1alpha1.PrepareData).
		setJobName(fmt.Sprintf("%s-%d", populatePVC.Name, index)).
		addLabel(DataProtectionPopulatePVCLabelKey, populatePVC.Name).
		setImage(backupSet.ActionSet.Spec.Restore.PrepareData.Image).
		setCommand(backupSet.ActionSet.Spec.Restore.PrepareData.Command).
		setServiceAccount(r.WorkerServiceAccount).
		attachBackupRepo().
		addCommonEnv(sourceTargetPodName)
	volume, volumeMount, err := jobBuilder.buildPVCVolumeAndMount(*prepareDataConfig.DataSourceRef, populatePVC.Name, "dp-claim")
	if err != nil {
		return nil, err
	}
	job := jobBuilder.addToSpecificVolumesAndMounts(volume, volumeMount).build()
	return job, nil
}

// BuildPostReadyActionJobs builds the post ready jobs.
func (r *RestoreManager) BuildPostReadyActionJobs(reqCtx intctrlutil.RequestCtx, cli client.Client, backupSet BackupActionSet, target *dpv1alpha1.BackupStatusTarget, step int) ([]*batchv1.Job, error) {
	readyConfig := r.Restore.Spec.ReadyConfig
	if readyConfig == nil {
		return nil, nil
	}
	if !backupSet.ActionSet.HasPostReadyStage() {
		return nil, nil
	}
	backupRepo, err := r.prepareBackupRepo(reqCtx, cli, backupSet)
	if err != nil {
		return nil, err
	}
	actionSpec := backupSet.ActionSet.Spec.Restore.PostReady[step]
	getTargetPodList := func(labelSelector metav1.LabelSelector, msgKey string) (*corev1.PodList, error) {
		targetPodList, err := utils.GetPodListByLabelSelector(reqCtx, cli, &labelSelector)
		if err != nil {
			return nil, err
		}
		if len(targetPodList.Items) == 0 {
			return nil, fmt.Errorf("can not found any pod by spec.readyConfig.%s.target.podSelector", msgKey)
		}
		return targetPodList, nil
	}

	buildJobName := func(index int) string {
		jobName := fmt.Sprintf("restore-post-ready-%s-%s-%d-%d", r.Restore.UID[:8], backupSet.Backup.Name, step, index)
		return cutJobName(jobName)
	}
	jobBuilder := newRestoreJobBuilder(r.Restore, backupSet, backupRepo, dpv1alpha1.PostReady)
	buildJobsForJobAction := func() ([]*batchv1.Job, error) {
		jobAction := r.Restore.Spec.ReadyConfig.JobAction
		if jobAction == nil {
			return nil, intctrlutil.NewFatalError("spec.readyConfig.jobAction can not be empty")
		}
		podSelector := jobAction.Target.PodSelector
		if podSelector.LabelSelector == nil {
			return nil, intctrlutil.NewFatalError("spec.readyConfig.jobAction.podSelector.labelSelector can not be empty")
		}
		targetPodList, err := getTargetPodList(*podSelector.LabelSelector, "jobAction")
		if err != nil {
			return nil, err
		}
		sort.Sort(intctrlutil.ByPodName(targetPodList.Items))
		buildJob := func(targetPod *corev1.Pod, sourceTargetPodName string, index int) *batchv1.Job {
			if boolptr.IsSetToTrue(actionSpec.Job.RunOnTargetPodNode) {
				jobBuilder.resetSpecificVolumesAndMounts()
				jobBuilder.setNodeNameToNodeSelector(targetPod.Spec.NodeName)
				// mount the targe pod's volumes when RunOnTargetPodNode is true
				for _, volumeMount := range jobAction.Target.VolumeMounts {
					for _, volume := range targetPod.Spec.Volumes {
						if volume.Name != volumeMount.Name {
							continue
						}
						jobBuilder.addToSpecificVolumesAndMounts(&volume, &volumeMount)
					}
				}
			}
			return jobBuilder.setImage(actionSpec.Job.Image).
				setJobName(buildJobName(index)).
				addCommonEnv(sourceTargetPodName).
				attachBackupRepo().
				setCommand(actionSpec.Job.Command).
				setToleration(targetPod.Spec.Tolerations).
				addTargetPodAndCredentialEnv(targetPod, readyConfig.ConnectionCredential, &target.BackupTarget).
				setServiceAccount(r.WorkerServiceAccount).
				build()
		}

		if podSelector.Strategy == dpv1alpha1.PodSelectionStrategyAny {
			targetPod := utils.GetFirstIndexRunningPod(targetPodList)
			if targetPod == nil {
				return nil, fmt.Errorf("can not found any running pod by spec.readyConfig.jobAction.target.podSelector")
			}
			targetPodList.Items = []corev1.Pod{*targetPod}
		}
		var jobs []*batchv1.Job
		for i := range targetPodList.Items {
			sourceTargetPodName, err := GetSourcePodNameFromTarget(target, jobAction.RequiredPolicyForAllPodSelection, i)
			if err != nil {
				return nil, err
			}
			if target.PodSelector.Strategy == dpv1alpha1.PodSelectionStrategyAll && sourceTargetPodName == "" {
				// no need to recover the volume when the pod selection policy is 'All' and sourceTargetPodName is not found.
				continue
			}
			jobs = append(jobs, buildJob(&targetPodList.Items[i], sourceTargetPodName, i))
		}
		return jobs, nil
	}

	buildJobsForExecAction := func() ([]*batchv1.Job, error) {
		execAction := r.Restore.Spec.ReadyConfig.ExecAction
		if execAction == nil {
			return nil, intctrlutil.NewFatalError("spec.readyConfig.execAction can not be empty")
		}
		targetPodList, err := getTargetPodList(execAction.Target.PodSelector, "execAction")
		if err != nil {
			return nil, err
		}
		var restoreJobs []*batchv1.Job
		for i := range targetPodList.Items {
			containerName := actionSpec.Exec.Container
			if containerName == "" {
				containerName = targetPodList.Items[i].Spec.Containers[0].Name
			}
			args := append([]string{"-n", targetPodList.Items[i].Namespace, "exec", targetPodList.Items[i].Name, "-c", containerName, "--"}, actionSpec.Exec.Command...)
			jobBuilder.setImage(viper.GetString(constant.KBToolsImage)).setCommand([]string{"kubectl"}).setArgs(args).
				setJobName(buildJobName(i)).
				setToleration(targetPodList.Items[i].Spec.Tolerations)
			job := jobBuilder.build()
			// create exec job in kubeblocks namespace for security
			kbInstalledNamespace := viper.GetString(constant.CfgKeyCtrlrMgrNS)
			if kbInstalledNamespace != "" {
				job.Namespace = kbInstalledNamespace
				// use the dedicated ServiceAccount for executing "kubectl exec"
				job.Spec.Template.Spec.ServiceAccountName = viper.GetString(dptypes.CfgKeyExecWorkerServiceAccountName)
			}
			job.Labels[DataProtectionRestoreNamespaceLabelKey] = r.Restore.Namespace
			restoreJobs = append(restoreJobs, job)
		}
		return restoreJobs, nil
	}

	if actionSpec.Job != nil {
		return buildJobsForJobAction()
	}
	return buildJobsForExecAction()
}

func (r *RestoreManager) createPVCIfNotExist(
	reqCtx intctrlutil.RequestCtx,
	cli client.Client,
	claimMetadata metav1.ObjectMeta,
	claimSpec corev1.PersistentVolumeClaimSpec) error {
	claimMetadata.Namespace = reqCtx.Req.Namespace
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: claimMetadata,
		Spec:       claimSpec,
	}
	tmpPVC := &corev1.PersistentVolumeClaim{}
	if err := cli.Get(reqCtx.Ctx, types.NamespacedName{Name: claimMetadata.Name, Namespace: claimMetadata.Namespace}, tmpPVC); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		msg := fmt.Sprintf("created pvc %s/%s", pvc.Namespace, pvc.Name)
		r.Recorder.Event(r.Restore, corev1.EventTypeNormal, reasonCreateRestorePVC, msg)
		if err = cli.Create(reqCtx.Ctx, pvc); err != nil {
			return client.IgnoreAlreadyExists(err)
		}
	}
	return nil
}

// CreateJobsIfNotExist creates the jobs if not exist.
func (r *RestoreManager) CreateJobsIfNotExist(reqCtx intctrlutil.RequestCtx,
	cli client.Client,
	ownerObj client.Object,
	objs []*batchv1.Job) ([]*batchv1.Job, error) {
	// creates jobs if not exist
	var fetchedJobs []*batchv1.Job
	for i := range objs {
		if objs[i] == nil {
			continue
		}
		fetchedJob := &batchv1.Job{}
		if err := cli.Get(reqCtx.Ctx, client.ObjectKeyFromObject(objs[i]), fetchedJob); err != nil {
			if !apierrors.IsNotFound(err) {
				return nil, err
			}
			if ownerObj.GetNamespace() == objs[i].Namespace {
				if err = controllerutil.SetControllerReference(ownerObj, objs[i], r.Schema); err != nil {
					return nil, err
				}
			}
			if err = cli.Create(reqCtx.Ctx, objs[i]); err != nil && !apierrors.IsAlreadyExists(err) {
				return nil, err
			}
			msg := fmt.Sprintf("created job %s/%s", objs[i].Namespace, objs[i].Name)
			r.Recorder.Event(r.Restore, corev1.EventTypeNormal, reasonCreateRestoreJob, msg)
			fetchedJobs = append(fetchedJobs, objs[i])
		} else {
			fetchedJobs = append(fetchedJobs, fetchedJob)
		}
	}
	return fetchedJobs, nil
}

// CheckJobsDone checks if jobs are completed or failed.
func (r *RestoreManager) CheckJobsDone(
	stage dpv1alpha1.RestoreStage,
	actionName string,
	backupSet BackupActionSet,
	fetchedJobs []*batchv1.Job) (bool, bool, error) {
	var (
		allJobFinished = true
		existFailedJob bool
	)
	restoreActions := &r.Restore.Status.Actions.PrepareData
	if stage == dpv1alpha1.PostReady {
		restoreActions = &r.Restore.Status.Actions.PostReady
	}
	// count the number of jobs that are completed, failed,
	// or have the normally terminated `restore` container
	finishedCount := 0
	for i := range fetchedJobs {
		statusAction := dpv1alpha1.RestoreStatusAction{
			Name:       actionName,
			ObjectKey:  BuildJobKeyForActionStatus(fetchedJobs[i].Name),
			BackupName: backupSet.Backup.Name,
		}
		done, _, errMsg := utils.IsJobFinished(fetchedJobs[i])
		switch {
		case errMsg != "":
			existFailedJob = true
			statusAction.Status = dpv1alpha1.RestoreActionFailed
			statusAction.Message = errMsg
			SetRestoreStatusAction(restoreActions, statusAction)
			finishedCount++
		case done:
			statusAction.Status = dpv1alpha1.RestoreActionCompleted
			SetRestoreStatusAction(restoreActions, statusAction)
			finishedCount++
		default:
			allJobFinished = false
			statusAction.Status = dpv1alpha1.RestoreActionProcessing
			SetRestoreStatusAction(restoreActions, statusAction)
			normalTerminated, err := r.CheckIfRestoreContainerTerminated(fetchedJobs[i])
			if err != nil {
				return false, false, err
			}
			if normalTerminated {
				finishedCount++
			}
		}
	}
	// wait until all `restore` containers are terminated normally or jobs are completed or failed
	if finishedCount == len(fetchedJobs) {
		for i := range fetchedJobs {
			err := r.StopManagerContainerByJob(fetchedJobs[i])
			if err != nil {
				return false, false, err
			}
		}
	}
	return allJobFinished, existFailedJob, nil
}

// CheckIfRestoreContainerTerminated checks if the `restore` container is terminated.
// If the `restore` container is terminated abnormally, stop the `restore manager` container.
func (r *RestoreManager) CheckIfRestoreContainerTerminated(job *batchv1.Job) (normalTerminated bool, err error) {
	podList, err := utils.GetAssociatedPodsOfJob(context.Background(), r.Client, job.Namespace, job.Name)
	if err != nil {
		return false, err
	}
	if len(podList.Items) == 0 {
		return false, nil
	}
	for i, pod := range podList.Items {
		for _, containerStatus := range pod.Status.ContainerStatuses {
			if containerStatus.Name != Restore {
				continue
			}
			terminatedState := containerStatus.State.Terminated
			if terminatedState != nil {
				if terminatedState.ExitCode != 0 {
					// stop `restore manager` container if the `restore` container is terminated abnormally
					if err := r.StopManagerContainer(&podList.Items[i]); err != nil {
						return false, err
					}
				} else {
					normalTerminated = true
				}
			}
		}
	}
	return normalTerminated, nil
}

// StopManagerContainerByJob stops the `restore manager` containers by the job.
func (r *RestoreManager) StopManagerContainerByJob(job *batchv1.Job) error {
	podList, err := utils.GetAssociatedPodsOfJob(context.Background(), r.Client, job.Namespace, job.Name)
	if err != nil {
		return err
	}
	for i := range podList.Items {
		err := r.StopManagerContainer(&podList.Items[i])
		if err != nil {
			return err
		}
	}
	return nil
}

// StopManagerContainer stops the `restore manager` container.
func (r *RestoreManager) StopManagerContainer(pod *corev1.Pod) error {
	modified := pod.DeepCopy()
	if modified.Annotations == nil {
		modified.Annotations = map[string]string{}
	}
	if val, ok := modified.Annotations[DataProtectionStopRestoreManagerAnnotationKey]; ok && val == "true" {
		return nil
	}
	// `restore manager` container will read this annotation to stop
	modified.Annotations[DataProtectionStopRestoreManagerAnnotationKey] = "true"
	if err := r.Client.Patch(context.Background(), modified, client.MergeFrom(pod)); err != nil {
		return client.IgnoreNotFound(err)
	}
	return nil
}

// Recalculation whether all actions have been completed.
func (r *RestoreManager) Recalculation(backupName, actionName string, allActionsFinished, existFailedAction *bool) {
	prepareDataConfig := r.Restore.Spec.PrepareDataConfig
	if !prepareDataConfig.IsSerialPolicy() {
		return
	}

	if *existFailedAction {
		// under the Serial policy, restore will be failed if any action is failed.
		*allActionsFinished = true
		return
	}
	var actionCount int
	for _, v := range r.Restore.Status.Actions.PrepareData {
		if v.Name == actionName && v.BackupName == backupName {
			actionCount += 1
		}
	}
	if actionCount != GetRestoreActionsCountForPrepareData(prepareDataConfig) {
		// if the number of actions is not equal to the number of target actions, the recovery has not yet ended
		*allActionsFinished = false
	}
}
