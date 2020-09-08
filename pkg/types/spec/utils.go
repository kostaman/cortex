/*
Copyright 2020 Cortex Labs, Inc.

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

package spec

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/cortexlabs/cortex/pkg/lib/aws"
	"github.com/cortexlabs/cortex/pkg/lib/errors"
	"github.com/cortexlabs/cortex/pkg/lib/files"
	"github.com/cortexlabs/cortex/pkg/lib/pointer"
	"github.com/cortexlabs/cortex/pkg/lib/sets/strset"
	"github.com/cortexlabs/cortex/pkg/lib/slices"
	s "github.com/cortexlabs/cortex/pkg/lib/strings"
	"github.com/cortexlabs/cortex/pkg/types/userconfig"
)

func FindDuplicateNames(apis []userconfig.API) []userconfig.API {
	names := make(map[string][]userconfig.API)

	for _, api := range apis {
		names[api.Name] = append(names[api.Name], api)
	}

	for name := range names {
		if len(names[name]) > 1 {
			return names[name]
		}
	}

	return nil
}

func checkDuplicateModelNames(models []CuratedModelResource) error {
	names := strset.New()

	for _, model := range models {
		if names.Has(model.Name) {
			return ErrorDuplicateModelNames(model.Name)
		}
		names.Add(model.Name)
	}

	return nil
}

func surgeOrUnavailableValidator(str string) (string, error) {
	if strings.HasSuffix(str, "%") {
		parsed, ok := s.ParseInt32(strings.TrimSuffix(str, "%"))
		if !ok {
			return "", ErrorInvalidSurgeOrUnavailable(str)
		}
		if parsed < 0 || parsed > 100 {
			return "", ErrorInvalidSurgeOrUnavailable(str)
		}
	} else {
		parsed, ok := s.ParseInt32(str)
		if !ok {
			return "", ErrorInvalidSurgeOrUnavailable(str)
		}
		if parsed < 0 {
			return "", ErrorInvalidSurgeOrUnavailable(str)
		}
	}

	return str, nil
}

// Verifies if modelName is found in models slice.
func isModelNameIn(models []userconfig.ModelResource, modelName string) bool {
	for _, model := range models {
		if model.Name == modelName {
			return true
		}
	}
	return false
}

// Returns absolute path of "path" based on "basedir".
func absolutePath(path, basedir string) (string, error) {
	var err error
	if strings.HasPrefix(path, "~/") {
		path, err = files.EscapeTilde(path)
		if err != nil {
			return "", err
		}
	} else {
		path = files.RelToAbsPath(path, basedir)
	}

	return path, nil
}

func modelResourceToCurated(modelResources []userconfig.ModelResource, predictorType userconfig.PredictorType, projectFiles ProjectFiles) ([]CuratedModelResource, error) {
	models := []CuratedModelResource{}
	var err error
	for _, model := range modelResources {
		isS3Path := strings.HasPrefix(model.ModelPath, "s3://")
		if !isS3Path {
			model.ModelPath, err = absolutePath(model.ModelPath, projectFiles.ProjectDir())
			if err != nil {
				return []CuratedModelResource{}, err
			}
		}

		if predictorType == userconfig.ONNXPredictorType && strings.HasSuffix(strings.TrimSuffix(model.ModelPath, "/"), ".onnx") {
			model.ModelPath = strings.TrimSuffix(model.ModelPath, "/")
			model.Name = strings.TrimSuffix(model.Name, ".onnx")
		} else {
			model.ModelPath = s.EnsureSuffix(model.ModelPath, "/")
		}

		models = append(models, CuratedModelResource{
			ModelResource: userconfig.ModelResource{
				Name:         model.Name,
				ModelPath:    model.ModelPath,
				SignatureKey: model.SignatureKey,
			},
			S3Path: isS3Path,
		})
	}

	return models, nil
}

// Retrieves the model objects found in the S3/local path directory.
//
// The model name is determined from the objects' names found in the path directory.
// Path can either be an S3 path or a local system path - in the latter case, the returned paths will be in absolute form.
func retrieveModelsResourcesFromPath(path string, projectFiles ProjectFiles, awsClient *aws.Client) ([]userconfig.ModelResource, error) {
	models := []userconfig.ModelResource{}

	if aws.IsValidS3Path(path) {
		awsClientForBucket, err := aws.NewFromClientS3Path(path, awsClient)
		if err != nil {
			return models, err
		}

		if isDir, err := awsClientForBucket.IsS3PathDir(path); err != nil {
			return models, err
		} else if isDir {
			modelPaths, err := awsClientForBucket.GetNLevelsDeepFromS3Path(path, 1, false, pointer.Int64(20000))
			if err != nil {
				return models, err
			}
			var bucket string
			bucket, _, err = aws.SplitS3Path(path)
			if err != nil {
				return models, err
			}

			for _, modelPath := range modelPaths {
				models = append(models, userconfig.ModelResource{
					Name:      filepath.Base(modelPath),
					ModelPath: aws.S3Path(bucket, modelPath),
				})
			}
		} else {
			return models, ErrorS3DirNotFound(path)
		}

	} else {
		var err error
		path, err = absolutePath(path, projectFiles.ProjectDir())
		if err != nil {
			return models, err
		}

		var fi os.FileInfo
		fi, err = os.Stat(path)
		if err != nil {
			return models, ErrorInvalidPath(path)
		}
		if !fi.Mode().IsDir() {
			return models, ErrorInvalidDirPath(path)
		}

		var file *os.File
		file, err = os.Open(path)
		if err != nil {
			return models, err
		}

		var modelObjects []string
		modelObjects, err = file.Readdirnames(0)
		if err != nil {
			return models, err
		}

		for _, modelName := range modelObjects {
			models = append(models, userconfig.ModelResource{
				Name:      modelName,
				ModelPath: filepath.Join(path, modelName),
			})
		}
	}

	return models, nil
}

// getTFServingVersionsFromS3Path checks that the path contains a valid S3 directory for (Neuron) TensorFlow models:
//
// For TensorFlow models:
// - model-name
// 		- 1523423423/ (version prefix, usually a timestamp)
//			- saved_model.pb
// 			- variables/
//				- variables.index
//				- variables.data-00000-of-00001 (there are a variable number of these files)
// 		- 2434389194/ (version prefix, usually a timestamp)
// 			- saved_model.pb
//			- variables/
//				- variables.index
//				- variables.data-00000-of-00001 (there are a variable number of these files)
//   ...
//
// For Neuron TensorFlow models:
// - model-name
// 		- 1523423423/ (version prefix, usually a timestamp)
// 			- saved_model.pb
// 		- 2434389194/ (version prefix, usually a timestamp)
//			- saved_model.pb
// 		...
//
func getTFServingVersionsFromS3Path(commonModelPrefix string, modelPaths []string, isNeuronExport bool, awsClientForBucket *aws.Client) ([]int64, error) {
	if len(modelPaths) == 0 {
		return []int64{}, ErrorInvalidTensorFlowModelPath(commonModelPrefix, modelPaths, isNeuronExport)
	}

	versions := []int64{}
	for _, object := range modelPaths {
		keyParts := strings.Split(object, "/")
		versionStr := keyParts[len(keyParts)-1]
		version, err := strconv.ParseInt(versionStr, 10, 64)
		if err != nil {
			return []int64{}, ErrorInvalidTensorFlowModelPath(commonModelPrefix, modelPaths, isNeuronExport)
		}

		modelVersionPath := aws.JoinS3Path(commonModelPrefix, versionStr)
		if err := validateTFServingS3ModelDir(commonModelPrefix, modelPaths, modelVersionPath, isNeuronExport, awsClientForBucket); err != nil {
			return []int64{}, err
		}
		versions = append(versions, version)
	}

	return slices.UniqueInt64(versions), nil
}

func validateTFServingS3ModelDir(commonModelPrefix string, modelPaths []string, modelVersionPath string, isNeuronExport bool, awsClientForBucket *aws.Client) error {
	if yes, err := awsClientForBucket.IsS3PathDir(modelVersionPath); err != nil {
		return err
	} else if !yes {
		return ErrorInvalidTensorFlowModelPath(commonModelPrefix, modelPaths, isNeuronExport)
	}

	if isNeuronExport {
		if !isValidNeuronTensorFlowS3Directory(modelVersionPath, awsClientForBucket) {
			return ErrorInvalidTensorFlowModelPath(commonModelPrefix, modelPaths, isNeuronExport)
		}
	} else {
		if !isValidTensorFlowS3Directory(modelVersionPath, awsClientForBucket) {
			return ErrorInvalidTensorFlowModelPath(commonModelPrefix, modelPaths, isNeuronExport)
		}
	}

	return nil
}

// isValidTensorFlowS3Directory checks that the path contains a valid S3 directory for TensorFlow models
// Must contain the following structure:
// - 1523423423/ (version prefix, usually a timestamp)
// 		- saved_model.pb
//		- variables/
//			- variables.index
//			- variables.data-00000-of-00001 (there are a variable number of these files)
func isValidTensorFlowS3Directory(path string, awsClientForBucket *aws.Client) bool {
	if valid, err := awsClientForBucket.IsS3PathFile(
		aws.JoinS3Path(path, "saved_model.pb"),
		aws.JoinS3Path(path, "variables/variables.index"),
	); err != nil || !valid {
		return false
	}

	if valid, err := awsClientForBucket.IsS3PathPrefix(
		aws.JoinS3Path(path, "variables/variables.data-00000-of"),
	); err != nil || !valid {
		return false
	}

	return true
}

// isValidNeuronTensorFlowS3Directory checks that the path contains a valid S3 directory for Neuron TensorFlow models
// Must contain the following structure:
// - 1523423423/ (version prefix, usually a timestamp)
// 		- saved_model.pb
func isValidNeuronTensorFlowS3Directory(path string, awsClient *aws.Client) bool {
	if valid, err := awsClient.IsS3PathFile(
		aws.JoinS3Path(path, "saved_model.pb"),
	); err != nil || !valid {
		return false
	}

	return true
}

// GetTFServingVersionsFromLocalPath checks that the path contains a valid local directory for TensorFlow models:
// - model-name
// 		- 1523423423/ (version prefix, usually a timestamp)
//			- saved_model.pb
// 			- variables/
//				- variables.index
//				- variables.data-00000-of-00001 (there are a variable number of these files)
// 		- 2434389194/ (version prefix, usually a timestamp)
// 			- saved_model.pb
//			- variables/
//				- variables.index
//				- variables.data-00000-of-00001 (there are a variable number of these files)
//   ...
func getTFServingVersionsFromLocalPath(commonModelPrefix string, modelPaths []string) ([]int64, error) {
	if len(modelPaths) == 0 {
		return []int64{}, ErrorInvalidTensorFlowModelPath(commonModelPrefix, modelPaths, false)
	}

	basePathLength := len(slices.RemoveEmpties(strings.Split(commonModelPrefix, "/")))
	versions := []int64{}

	for _, modelPath := range modelPaths {
		pathParts := slices.RemoveEmpties(strings.Split(modelPath, "/"))
		versionStr := pathParts[basePathLength]
		version, err := strconv.ParseInt(versionStr, 10, 64)
		if err != nil {
			return []int64{}, ErrorInvalidTensorFlowModelPath(commonModelPrefix, modelPaths, false)
		}

		modelVersionPath := filepath.Join(commonModelPrefix, versionStr)
		if err := validateTFServingLocalModelDir(commonModelPrefix, modelPaths, modelVersionPath); err != nil {
			return []int64{}, err
		}

		versions = append(versions, version)
	}

	return slices.UniqueInt64(versions), nil
}

func validateTFServingLocalModelDir(commonModelPrefix string, modelPaths []string, modelVersionPath string) error {
	if !files.IsDir(modelVersionPath) {
		return ErrorInvalidTensorFlowModelPath(commonModelPrefix, modelPaths, false)
	}

	if yes, err := isValidTensorFlowLocalDirectory(modelVersionPath); !yes || err != nil {
		return ErrorInvalidTensorFlowModelPath(commonModelPrefix, modelPaths, false)
	}

	return nil
}

// isValidTensorFlowLocalDirectory checks that the path contains a valid local directory for TensorFlow models
// Must contain the following structure:
// - 1523423423/ (version prefix, usually a timestamp)
// 		- saved_model.pb
//		- variables/
//			- variables.index
//			- variables.data-00000-of-00001 (there are a variable number of these files)
func isValidTensorFlowLocalDirectory(path string) (bool, error) {
	paths, err := files.ListDirRecursive(path, true, files.IgnoreHiddenFiles, files.IgnoreHiddenFolders)
	if err != nil {
		return false, err
	}
	pathSet := strset.New(paths...)

	if !(pathSet.Has("saved_model.pb") && pathSet.Has("variables/variables.index")) {
		return false, nil
	}

	for _, path := range paths {
		if strings.HasPrefix(path, "variables/variables.data-00000-of") {
			return true, nil
		}
	}

	return false, nil
}

// TODO verify if adding imbricated directories pass the check for ONNX version checkers

// getONNXVersionsFromS3Path checks that the path contains a valid S3 directory for versioned ONNX models:
// - model-name
// 		- 1523423423/ (version prefix, usually a timestamp)
// 			- <model-name>.onnx
// 		- 2434389194/ (version prefix, usually a timestamp)
//			- <model-name>.onnx
// 		...
func getONNXVersionsFromS3Path(path string, awsClientForBucket *aws.Client) ([]int64, error) {
	objects, err := awsClientForBucket.GetNLevelsDeepFromS3Path(path, 1, false, pointer.Int64(1000))
	if err != nil {
		return []int64{}, err
	} else if len(objects) == 0 {
		return []int64{}, ErrorNoVersionsFoundForONNXModelPath(path)
	}

	versions := []int64{}
	bucket, _, err := aws.SplitS3Path(path)
	if err != nil {
		return []int64{}, err
	}
	for _, object := range objects {
		keyParts := strings.Split(object, "/")
		versionStr := keyParts[len(keyParts)-1]
		version, err := strconv.ParseInt(versionStr, 10, 64)
		if err != nil {
			return []int64{}, ErrorInvalidONNXModelPath(path)
		}

		modelVersionPath := aws.JoinS3Path(path, versionStr)
		if yes, err := awsClientForBucket.IsS3PathDir(modelVersionPath); err != nil {
			return []int64{}, err
		} else if !yes {
			return []int64{}, ErrorONNXModelVersionPathMustBeDir(path, aws.JoinS3Path(path, versionStr))
		}

		versionObjects, err := awsClientForBucket.GetNLevelsDeepFromS3Path(modelVersionPath, 1, false, pointer.Int64(1000))
		if err != nil {
			return []int64{}, err
		}

		numONNXFiles := 0
		for _, versionObject := range versionObjects {
			if !strings.HasSuffix(versionObject, ".onnx") {
				return []int64{}, ErrorInvalidONNXModelPath(path)
			}
			if yes, err := awsClientForBucket.IsS3PathFile(aws.S3Path(bucket, versionObject)); err != nil {
				return []int64{}, errors.Wrap(err, path)
			} else if !yes {
				return []int64{}, ErrorInvalidONNXModelPath(path)
			}
			numONNXFiles++
		}

		if numONNXFiles > 1 {
			return []int64{}, ErrorInvalidONNXModelPath(path)
		}

		versions = append(versions, version)
	}

	return slices.UniqueInt64(versions), nil
}

// GetONNXVersionsFromLocalPath checks that the path contains a valid local directory for versioned ONNX models:
// - model-name
// 		- 1523423423/ (version prefix, usually a timestamp)
// 			- <model-name>.onnx
// 		- 2434389194/ (version prefix, usually a timestamp)
//			- <model-name>.onnx
// 		...
func GetONNXVersionsFromLocalPath(path string) ([]int64, error) {
	dirPaths, err := files.ListDirRecursive(path, false, files.IgnoreHiddenFiles, files.IgnoreHiddenFolders)
	if err != nil {
		return []int64{}, err
	} else if len(dirPaths) == 0 {
		return []int64{}, ErrorNoVersionsFoundForONNXModelPath(path)
	}

	basePathLength := len(slices.RemoveEmpties(strings.Split(path, "/")))
	versions := []int64{}

	for _, dirPath := range dirPaths {
		pathParts := slices.RemoveEmpties(strings.Split(dirPath, "/"))
		versionStr := pathParts[basePathLength]
		version, err := strconv.ParseInt(versionStr, 10, 64)
		if err != nil {
			return []int64{}, ErrorInvalidONNXModelPath(path)
		}

		modelVersionPath := filepath.Join(path, versionStr)
		if !files.IsDir(modelVersionPath) {
			return []int64{}, ErrorONNXModelVersionPathMustBeDir(path, modelVersionPath)
		}

		var versionObjects []string
		if versionObjects, err = files.ListDir(modelVersionPath, false); err != nil {
			return []int64{}, err
		} else if len(versionObjects) == 0 {
			continue
		}

		numONNXFiles := 0
		for _, versionObject := range versionObjects {
			if !strings.HasSuffix(versionObject, ".onnx") || !files.IsFile(versionObject) {
				return []int64{}, ErrorInvalidONNXModelPath(path)
			}
			numONNXFiles++
		}

		if numONNXFiles > 1 {
			return []int64{}, ErrorInvalidONNXModelPath(path)
		}

		versions = append(versions, version)
	}

	return slices.UniqueInt64(versions), nil
}

// getPythonVersionsFromS3Path checks that the path contains a valid S3 directory for versioned Python models:
// - model-name
// 		- 1523423423/ (version prefix, usually a timestamp)
// 			- *
// 		- 2434389194/ (version prefix, usually a timestamp)
//			- *
// 		...
func getPythonVersionsFromS3Path(path string, awsClientForBucket *aws.Client) ([]int64, error) {
	objects, err := awsClientForBucket.GetNLevelsDeepFromS3Path(path, 1, false, pointer.Int64(1000))
	if err != nil {
		return []int64{}, err
	} else if len(objects) == 0 {
		return []int64{}, ErrorNoVersionsFoundForPythonModelPath(path)
	}

	versions := []int64{}
	for _, object := range objects {
		keyParts := strings.Split(object, "/")
		versionStr := keyParts[len(keyParts)-1]
		version, err := strconv.ParseInt(versionStr, 10, 64)
		if err != nil {
			return []int64{}, ErrorInvalidPythonModelPath(path)
		}

		modelVersionPath := aws.JoinS3Path(path, versionStr)
		if yes, err := awsClientForBucket.IsS3PathDir(modelVersionPath); err != nil {
			return []int64{}, err
		} else if !yes {
			return []int64{}, ErrorPythonModelVersionPathMustBeDir(path, aws.JoinS3Path(path, versionStr))
		}

		versions = append(versions, version)
	}

	return slices.UniqueInt64(versions), nil
}

// GetPythonVersionsFromLocalPath checks that the path contains a valid local directory for versioned Python models:
// - model-name
// 		- 1523423423/ (version prefix, usually a timestamp)
// 			- *
// 		- 2434389194/ (version prefix, usually a timestamp)
//			- *
// 		...
func GetPythonVersionsFromLocalPath(path string) ([]int64, error) {
	if !files.IsDir(path) {
		return []int64{}, ErrorInvalidDirPath(path)
	}
	dirPaths, err := files.ListDirRecursive(path, false, files.IgnoreHiddenFiles, files.IgnoreHiddenFolders)
	if err != nil {
		return []int64{}, err
	} else if len(dirPaths) == 0 {
		return []int64{}, ErrorNoVersionsFoundForPythonModelPath(path)
	}

	basePathLength := len(slices.RemoveEmpties(strings.Split(path, "/")))
	versions := []int64{}
	for _, dirPath := range dirPaths {
		pathParts := slices.RemoveEmpties(strings.Split(dirPath, "/"))
		versionStr := pathParts[basePathLength]
		version, err := strconv.ParseInt(versionStr, 10, 64)
		if err != nil {
			return []int64{}, ErrorInvalidPythonModelPath(path)
		}

		modelVersionPath := filepath.Join(path, versionStr)
		if !files.IsDir(modelVersionPath) {
			return []int64{}, ErrorPythonModelVersionPathMustBeDir(path, modelVersionPath)
		}

		if objects, err := files.ListDir(modelVersionPath, false); err != nil {
			return []int64{}, err
		} else if len(objects) == 0 {
			continue
		}

		versions = append(versions, version)
	}

	return slices.UniqueInt64(versions), nil
}
