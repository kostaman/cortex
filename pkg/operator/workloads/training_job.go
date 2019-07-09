/*
Copyright 2019 Cortex Labs, Inc.

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

package workloads

import (
	kbatch "k8s.io/api/batch/v1"
	kcore "k8s.io/api/core/v1"
	kresource "k8s.io/apimachinery/pkg/api/resource"
	kmeta "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cortexlabs/cortex/pkg/consts"
	"github.com/cortexlabs/cortex/pkg/lib/argo"
	"github.com/cortexlabs/cortex/pkg/lib/k8s"
	"github.com/cortexlabs/cortex/pkg/lib/sets/strset"
	"github.com/cortexlabs/cortex/pkg/operator/api/context"
	"github.com/cortexlabs/cortex/pkg/operator/api/userconfig"
	"github.com/cortexlabs/cortex/pkg/operator/config"
)

func trainingJobSpec(
	ctx *context.Context,
	modelID string,
	workloadID string,
	tfCompute *userconfig.TFCompute,
) *kbatch.Job {

	resourceList := kcore.ResourceList{}
	limitsList := kcore.ResourceList{}
	resourceList[kcore.ResourceCPU] = tfCompute.CPU.Quantity
	if tfCompute.Mem != nil {
		resourceList[kcore.ResourceMemory] = tfCompute.Mem.Quantity
	}

	trainImage := config.Cortex.TFTrainImage
	if tfCompute.GPU > 0 {
		trainImage = config.Cortex.TFTrainImageGPU
		resourceList["nvidia.com/gpu"] = *kresource.NewQuantity(tfCompute.GPU, kresource.DecimalSI)
		limitsList["nvidia.com/gpu"] = *kresource.NewQuantity(tfCompute.GPU, kresource.DecimalSI)
	}

	spec := k8s.Job(&k8s.JobSpec{
		Name: workloadID,
		Labels: map[string]string{
			"appName":      ctx.App.Name,
			"workloadType": workloadTypeTrain,
			"workloadID":   workloadID,
		},
		PodSpec: k8s.PodSpec{
			Labels: map[string]string{
				"appName":      ctx.App.Name,
				"workloadType": workloadTypeTrain,
				"workloadID":   workloadID,
				"userFacing":   "true",
			},
			K8sPodSpec: kcore.PodSpec{
				RestartPolicy: "Never",
				Containers: []kcore.Container{
					{
						Name:            "train",
						Image:           trainImage,
						ImagePullPolicy: "Always",
						Args: []string{
							"--workload-id=" + workloadID,
							"--context=" + config.AWS.S3Path(ctx.Key),
							"--cache-dir=" + consts.ContextCacheDir,
							"--model=" + modelID,
						},
						Env:          k8s.AWSCredentials(),
						VolumeMounts: k8s.DefaultVolumeMounts(),
						Resources: kcore.ResourceRequirements{
							Requests: resourceList,
							Limits:   limitsList,
						},
					},
				},
				Volumes:            k8s.DefaultVolumes(),
				ServiceAccountName: "default",
			},
		},
		Namespace: config.Cortex.Namespace,
	})
	argo.EnableGC(spec)
	return spec
}

func trainingWorkloadSpecs(ctx *context.Context) ([]*WorkloadSpec, error) {
	modelsToTrain := make(map[string]*userconfig.TFCompute)
	for _, model := range ctx.Models {
		modelCached, err := checkResourceCached(model, ctx)
		if err != nil {
			return nil, err
		}
		if modelCached {
			continue
		}

		if tfCompute, ok := modelsToTrain[model.ID]; ok {
			modelsToTrain[model.ID] = userconfig.MaxTFCompute(tfCompute, model.Compute)
		} else {
			modelsToTrain[model.ID] = model.Compute
		}
	}

	var workloadSpecs []*WorkloadSpec
	for modelID, tfCompute := range modelsToTrain {
		workloadID := generateWorkloadID()
		workloadSpecs = append(workloadSpecs, &WorkloadSpec{
			WorkloadID:       workloadID,
			ResourceIDs:      strset.New(modelID),
			K8sSpecs:         []kmeta.Object{trainingJobSpec(ctx, modelID, workloadID, tfCompute)},
			K8sAction:        "create",
			SuccessCondition: k8s.JobSuccessCondition,
			FailureCondition: k8s.JobFailureCondition,
			WorkloadType:     workloadTypeTrain,
		})
	}

	return workloadSpecs, nil
}
