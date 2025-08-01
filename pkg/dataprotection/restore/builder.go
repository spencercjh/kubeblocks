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
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	dpv1alpha1 "github.com/apecloud/kubeblocks/apis/dataprotection/v1alpha1"
	"github.com/apecloud/kubeblocks/pkg/common"
	"github.com/apecloud/kubeblocks/pkg/constant"
	intctrlutil "github.com/apecloud/kubeblocks/pkg/controllerutil"
	dptypes "github.com/apecloud/kubeblocks/pkg/dataprotection/types"
	"github.com/apecloud/kubeblocks/pkg/dataprotection/utils"
	viper "github.com/apecloud/kubeblocks/pkg/viperx"
)

type restoreJobBuilder struct {
	restore            *dpv1alpha1.Restore
	stage              dpv1alpha1.RestoreStage
	backupSet          BackupActionSet
	backupRepo         *dpv1alpha1.BackupRepo
	buildWithRepo      bool
	env                []corev1.EnvVar
	envFrom            []corev1.EnvFromSource
	commonVolumes      []corev1.Volume
	commonVolumeMounts []corev1.VolumeMount
	// specificVolumes should be rebuilt for each job.
	specificVolumes []corev1.Volume
	// specificVolumeMounts should be rebuilt for each job.
	specificVolumeMounts []corev1.VolumeMount
	image                string
	command              []string
	args                 []string
	tolerations          []corev1.Toleration
	nodeSelector         map[string]string
	jobName              string
	labels               map[string]string
	serviceAccount       string
}

func newRestoreJobBuilder(restore *dpv1alpha1.Restore, backupSet BackupActionSet, backupRepo *dpv1alpha1.BackupRepo, stage dpv1alpha1.RestoreStage) *restoreJobBuilder {
	return &restoreJobBuilder{
		restore:            restore,
		backupSet:          backupSet,
		backupRepo:         backupRepo,
		stage:              stage,
		commonVolumes:      []corev1.Volume{},
		commonVolumeMounts: []corev1.VolumeMount{},
		labels:             BuildRestoreLabels(restore.Name),
	}
}

func (r *restoreJobBuilder) buildPVCVolumeAndMount(
	claim dpv1alpha1.VolumeConfig,
	claimName,
	identifier string) (*corev1.Volume, *corev1.VolumeMount, error) {
	volumeName := fmt.Sprintf("%s-%s", identifier, claimName)
	volume := &corev1.Volume{
		Name:         volumeName,
		VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claimName}},
	}
	volumeMount := &corev1.VolumeMount{Name: volumeName}
	if claim.MountPath != "" {
		volumeMount.MountPath = claim.MountPath
		return volume, volumeMount, nil
	}
	mountPath := getMountPathWithSourceVolume(r.backupSet.Backup, claim.VolumeSource)
	if mountPath != "" {
		volumeMount.MountPath = mountPath
		return volume, volumeMount, nil
	}

	if r.backupSet.UseVolumeSnapshot && !r.backupSet.ActionSet.HasPrepareDataStage() {
		return nil, nil, nil
	}
	return nil, nil, intctrlutil.NewFatalError(fmt.Sprintf(`unable to find the mountPath corresponding to volumeSource "%s" from status.backupMethod.targetVolumes.volumeMounts of backup "%s"`,
		claim.VolumeSource, r.backupSet.Backup.Name))
}

// addToCommonVolumesAndMounts adds the volume and volumeMount to common volumes and volumeMounts slice.
func (r *restoreJobBuilder) addToCommonVolumesAndMounts(volume *corev1.Volume, volumeMount *corev1.VolumeMount) *restoreJobBuilder {
	if volume != nil {
		r.commonVolumes = append(r.commonVolumes, *volume)
	}
	if volumeMount != nil {
		r.commonVolumeMounts = append(r.commonVolumeMounts, *volumeMount)
	}
	return r
}

// resetSpecificVolumesAndMounts resets the specific volumes and volumeMounts slice.
func (r *restoreJobBuilder) resetSpecificVolumesAndMounts() {
	r.specificVolumes = []corev1.Volume{}
	r.specificVolumeMounts = []corev1.VolumeMount{}
}

// resetSpecificLabels resets the specific labels.
func (r *restoreJobBuilder) resetSpecificLabels() {
	r.labels = BuildRestoreLabels(r.restore.Name)
}

// addToSpecificVolumesAndMounts adds the volume and volumeMount to specific volumes and volumeMounts slice.
func (r *restoreJobBuilder) addToSpecificVolumesAndMounts(volume *corev1.Volume, volumeMount *corev1.VolumeMount) *restoreJobBuilder {
	if volume != nil {
		r.specificVolumes = append(r.specificVolumes, *volume)
	}
	if volumeMount != nil {
		r.specificVolumeMounts = append(r.specificVolumeMounts, *volumeMount)
	}
	return r
}

func (r *restoreJobBuilder) setImage(image string) *restoreJobBuilder {
	r.image = image
	return r
}

func (r *restoreJobBuilder) setCommand(command []string) *restoreJobBuilder {
	r.command = command
	return r
}

func (r *restoreJobBuilder) setArgs(args []string) *restoreJobBuilder {
	r.args = args
	return r
}

func (r *restoreJobBuilder) setToleration(tolerations []corev1.Toleration) *restoreJobBuilder {
	r.tolerations = tolerations
	return r
}

func (r *restoreJobBuilder) setNodeNameToNodeSelector(nodeName string) *restoreJobBuilder {
	r.nodeSelector = map[string]string{
		corev1.LabelHostname: nodeName,
	}
	return r
}

func (r *restoreJobBuilder) setJobName(jobName string) *restoreJobBuilder {
	r.jobName = jobName
	return r
}

func (r *restoreJobBuilder) addLabel(key, value string) *restoreJobBuilder {
	if r.labels == nil {
		r.labels = map[string]string{}
	}
	if _, ok := r.labels[key]; ok {
		return r
	}
	r.labels[key] = value
	return r
}

func (r *restoreJobBuilder) setServiceAccount(serviceAccount string) *restoreJobBuilder {
	r.serviceAccount = serviceAccount
	return r
}

func (r *restoreJobBuilder) attachBackupRepo() *restoreJobBuilder {
	r.buildWithRepo = true
	return r
}

// addCommonEnv adds the common envs for each restore job.
func (r *restoreJobBuilder) addCommonEnv(sourceTargetPodName string) *restoreJobBuilder {
	backup := r.backupSet.Backup
	backupName := backup.Name
	// add backupName env
	r.env = []corev1.EnvVar{{Name: dptypes.DPBackupName, Value: backupName}}
	// add common env
	filePath := r.backupSet.Backup.Status.Path
	if filePath != "" {
		r.env = append(r.env, BackupFilePathEnv(filePath, r.restore.Spec.Backup.SourceTargetName, sourceTargetPodName)...)
	}
	if r.backupSet.BaseBackup != nil {
		r.env = append(r.env, corev1.EnvVar{Name: dptypes.DPBaseBackupName, Value: r.backupSet.BaseBackup.Name})
	}
	if len(r.backupSet.AncestorIncrementalBackups) > 0 {
		ancestorIncrementalBackupNames := []string{}
		for _, backup := range r.backupSet.AncestorIncrementalBackups {
			ancestorIncrementalBackupNames = append(ancestorIncrementalBackupNames, backup.Name)
		}
		r.env = append(r.env, corev1.EnvVar{Name: dptypes.DPAncestorIncrementalBackupNames, Value: strings.Join(ancestorIncrementalBackupNames, ",")})
	}
	// add time env
	actionSetEnv := r.backupSet.ActionSet.Spec.Env
	timeFormat := getTimeFormat(actionSetEnv)
	appendTimeEnv := func(envName, envTimestampName, timeZone string, targetTime *metav1.Time) {
		if targetTime.IsZero() {
			return
		}
		targetTime, _ = transformTimeWithZone(targetTime, timeZone)
		if envName != "" {
			r.env = append(r.env, corev1.EnvVar{Name: envName, Value: targetTime.Format(timeFormat)})
		}
		if envTimestampName != "" {
			r.env = append(r.env, corev1.EnvVar{Name: envTimestampName, Value: strconv.FormatInt(targetTime.Unix(), 10)})
		}
	}
	appendTimeEnv(dptypes.DPBackupStopTime, "", backup.GetTimeZone(), backup.GetEndTime())
	if r.backupSet.BaseBackup != nil {
		appendTimeEnv(DPBaseBackupStartTime, DPBaseBackupStartTimestamp, r.backupSet.BaseBackup.GetTimeZone(), r.backupSet.BaseBackup.GetStartTime())
		appendTimeEnv(DPBaseBackupStopTime, DPBaseBackupStopTimestamp, r.backupSet.BaseBackup.GetTimeZone(), r.backupSet.BaseBackup.GetEndTime())
	}
	if r.restore.Spec.RestoreTime != "" {
		restoreTime, _ := time.Parse(time.RFC3339, r.restore.Spec.RestoreTime)
		appendTimeEnv(DPRestoreTime, DPRestoreTimestamp, backup.GetTimeZone(), &metav1.Time{Time: restoreTime})
	}
	// append restore parameters env
	if r.restore != nil {
		r.env = append(r.env, utils.BuildEnvByParameters(r.restore.Spec.Parameters)...)
	}
	// append actionSet env
	r.env = append(r.env, actionSetEnv...)
	backupMethod := r.backupSet.Backup.Status.BackupMethod
	if backupMethod != nil && len(backupMethod.Env) > 0 {
		r.env = utils.MergeEnv(r.env, backupMethod.Env)
	}
	// merge the restore env
	r.env = utils.MergeEnv(r.env, r.restore.Spec.Env)
	return r
}

func (r *restoreJobBuilder) addTargetPodAndCredentialEnv(pod *corev1.Pod,
	connectionCredential *dpv1alpha1.ConnectionCredential,
	target *dpv1alpha1.BackupTarget) *restoreJobBuilder {
	if pod == nil {
		return r
	}
	var env []corev1.EnvVar
	// Note: now only add the first container envs.
	if len(pod.Spec.Containers) != 0 {
		env = pod.Spec.Containers[0].Env
		r.envFrom = pod.Spec.Containers[0].EnvFrom
	}
	addDBHostEnv := func() {
		env = append(env, corev1.EnvVar{Name: dptypes.DPDBHost, Value: intctrlutil.BuildPodHostDNS(pod)})
	}
	addDBPortEnv := func() {
		portEnv, err := utils.GetDPDBPortEnv(pod, target.ContainerPort)
		if err != nil {
			// fallback to use the first port of the pod
			portEnv, _ = utils.GetDPDBPortEnv(pod, nil)
		}
		if portEnv != nil {
			env = append(env, *portEnv)
		}
	}
	if connectionCredential == nil {
		addDBHostEnv()
		addDBPortEnv()
	} else {
		appendEnvFromSecret := func(envName, keyName string) {
			if keyName == "" {
				return
			}
			env = append(env, corev1.EnvVar{Name: envName, ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: connectionCredential.SecretName,
					},
					Key: keyName,
				},
			}})
		}
		appendEnvFromSecret(dptypes.DPDBUser, connectionCredential.UsernameKey)
		appendEnvFromSecret(dptypes.DPDBPassword, connectionCredential.PasswordKey)
		if connectionCredential.PortKey != "" {
			appendEnvFromSecret(dptypes.DPDBPort, connectionCredential.PortKey)
		} else {
			addDBPortEnv()
		}
		if connectionCredential.HostKey != "" {
			appendEnvFromSecret(dptypes.DPDBHost, connectionCredential.HostKey)
		} else {
			addDBHostEnv()
		}
	}
	r.env = utils.MergeEnv(r.env, env)
	return r
}

// builderRestoreJobName builds restore job name.
func (r *restoreJobBuilder) builderRestoreJobName(jobIndex int) string {
	jobName := fmt.Sprintf("restore-%s-%s-%s-%d", strings.ToLower(string(r.stage)), r.restore.UID[:8], r.backupSet.Backup.Name, jobIndex)
	return cutJobName(jobName)
}

// build the restore job by this builder.
func (r *restoreJobBuilder) build() *batchv1.Job {
	if r.jobName == "" {
		r.jobName = r.builderRestoreJobName(0)
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      r.jobName,
			Namespace: r.restore.Namespace,
			Labels:    r.labels,
		},
	}
	podSpec := job.Spec.Template.Spec
	// 1. set pod spec
	runUser := int64(0)
	podSpec.SecurityContext = &corev1.PodSecurityContext{
		RunAsUser: &runUser,
	}
	podSpec.RestartPolicy = corev1.RestartPolicyNever
	if r.stage == dpv1alpha1.PrepareData {
		// set scheduling spec
		schedulingSpec := r.restore.Spec.PrepareDataConfig.SchedulingSpec
		podSpec.Tolerations = schedulingSpec.Tolerations
		podSpec.Affinity = schedulingSpec.Affinity
		podSpec.NodeSelector = schedulingSpec.NodeSelector
		podSpec.NodeName = schedulingSpec.NodeName
		podSpec.SchedulerName = schedulingSpec.SchedulerName
		podSpec.TopologySpreadConstraints = schedulingSpec.TopologySpreadConstraints
	} else {
		podSpec.Tolerations = r.tolerations
		podSpec.NodeSelector = r.nodeSelector
	}
	r.specificVolumes = append(r.specificVolumes, r.commonVolumes...)
	podSpec.Volumes = r.specificVolumes
	podSpec.ServiceAccountName = r.serviceAccount

	job.Spec.Template.Spec = podSpec
	job.Spec.Template.ObjectMeta = metav1.ObjectMeta{
		Labels: r.labels,
	}
	if r.restore.Spec.BackoffLimit != nil {
		job.Spec.BackoffLimit = r.restore.Spec.BackoffLimit
	} else {
		job.Spec.BackoffLimit = &defaultBackoffLimit
	}

	// 2. set restore container
	r.specificVolumeMounts = append(r.specificVolumeMounts, r.commonVolumeMounts...)
	// expand the image value with the env variables.
	image := common.Expand(r.image, common.MappingFuncFor(utils.CovertEnvToMap(r.env)))
	container := corev1.Container{
		Name:            Restore,
		Resources:       r.restore.Spec.ContainerResources,
		Env:             r.env,
		EnvFrom:         r.envFrom,
		VolumeMounts:    r.specificVolumeMounts,
		Command:         r.command,
		Args:            r.args,
		Image:           intctrlutil.ReplaceImageRegistry(image),
		ImagePullPolicy: corev1.PullIfNotPresent,
	}

	buildBackupExtrasDownward := func() {
		extras := r.backupSet.Backup.Status.Extras
		if len(extras) == 0 {
			return
		}
		volumeName := "downward-volume"
		if job.Spec.Template.ObjectMeta.Annotations == nil {
			job.Spec.Template.ObjectMeta.Annotations = map[string]string{}
		}
		data, _ := json.Marshal(extras)
		job.Spec.Template.ObjectMeta.Annotations[DataProtectionBackupExtrasLabelKey] = string(data)
		job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, corev1.Volume{
			Name: volumeName,
			VolumeSource: corev1.VolumeSource{
				DownwardAPI: &corev1.DownwardAPIVolumeSource{
					Items: []corev1.DownwardAPIVolumeFile{
						{
							Path: "status_extras",
							FieldRef: &corev1.ObjectFieldSelector{
								FieldPath: "metadata.annotations['" + DataProtectionBackupExtrasLabelKey + "']",
							},
						},
					},
				},
			},
		})
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name:      volumeName,
			MountPath: "/dp_downward/",
		})
	}
	// downward backup.status.extras to volumes
	buildBackupExtrasDownward()

	intctrlutil.InjectZeroResourcesLimitsIfEmpty(&container)
	job.Spec.Template.Spec.Containers = []corev1.Container{container}
	controllerutil.AddFinalizer(job, dptypes.DataProtectionFinalizerName)

	// 3. inject restore manager
	// `restore manager` container waits until all restore containers from every restore job have finished,
	// by monitoring an `dataprotection.kubeblocks.io/stop-restore-manager` annotation signal added by
	// the restore controller after all restore containers finish.
	//
	// To guarantee that the recovered PVCs/PVs and pods are scheduled correctly, we specify the same
	// scheduling policy for job pods. However, the scheduler might not consider the restore job pod
	// when scheduling other pods if it is completed too quickly, which may lead to incorrect scheduling.
	// This container ensures that all job pods are considered by the scheduler.
	r.InjectManagerContainer(&job.Spec.Template.Spec)

	// 4. inject datasafed if needed
	if r.buildWithRepo {
		mountPath := "/backupdata"
		kopiaRepoPath := r.backupSet.Backup.Status.KopiaRepoPath
		encryptionConfig := r.backupSet.Backup.Status.EncryptionConfig
		if r.backupRepo != nil {
			utils.InjectDatasafed(&job.Spec.Template.Spec, r.backupRepo, mountPath,
				encryptionConfig, kopiaRepoPath)
		} else if pvcName := r.backupSet.Backup.Status.PersistentVolumeClaimName; pvcName != "" {
			// If the backup object was created in an old version that doesn't have the backupRepo field,
			// use the PVC name field as a fallback.
			utils.InjectDatasafedWithPVC(&job.Spec.Template.Spec, pvcName, mountPath, kopiaRepoPath)
		}
	}
	return job
}

func (r *restoreJobBuilder) InjectManagerContainer(podSpec *corev1.PodSpec) {
	container := corev1.Container{
		Name:            restoreManagerContainerName,
		Image:           viper.GetString(constant.KBToolsImage),
		ImagePullPolicy: corev1.PullPolicy(viper.GetString(constant.KBImagePullPolicy)),
		Resources:       corev1.ResourceRequirements{Limits: nil, Requests: nil},
		Command:         []string{"sh", "-c"},
	}
	intctrlutil.InjectZeroResourcesLimitsIfEmpty(&container)

	checkIntervalSeconds := int32(1)
	volumeName := "downward-volume"
	mountPath := "/dp_downward"
	fileName := "stop_restore_manager"

	podSpec.Volumes = append(podSpec.Volumes, corev1.Volume{
		Name: volumeName,
		VolumeSource: corev1.VolumeSource{
			DownwardAPI: &corev1.DownwardAPIVolumeSource{
				Items: []corev1.DownwardAPIVolumeFile{
					{
						Path: fileName,
						FieldRef: &corev1.ObjectFieldSelector{
							FieldPath: fmt.Sprintf("metadata.annotations['%s']", DataProtectionStopRestoreManagerAnnotationKey),
						},
					},
				},
			},
		},
	})
	container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
		Name:      volumeName,
		MountPath: mountPath,
	})

	buildSyncProgressCommand := func() string {
		return fmt.Sprintf(`
set -o errexit
set -o nounset

sleep_seconds="%d"
signal_file="%s"

if [ "$sleep_seconds" -le 0 ]; then
  sleep_seconds=2
fi

while true; do
  if [ -f "$signal_file" ] && [ "$(cat "$signal_file")" = "true" ]; then
    break
  fi
  echo "waiting for other restore workloads, sleep ${sleep_seconds}s"
  sleep "$sleep_seconds"
done

echo "restore manager stopped"
`, checkIntervalSeconds, filepath.Join(mountPath, fileName))
	}
	container.Args = []string{buildSyncProgressCommand()}
	podSpec.Containers = append(podSpec.Containers, container)
}
