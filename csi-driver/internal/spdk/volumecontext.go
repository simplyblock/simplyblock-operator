// Where a staged volume's context is kept, and the capacity rounding the
// controller service publishes with.
//
// The CSI spec passes VolumeContext to NodeStageVolume and to nothing after it,
// so the node service writes it beside the staging path and reads it back on
// unstage, publish, and expand. Capacity is rounded to whole GiB because the
// control plane provisions in GiB, and a volume reported smaller than it was
// asked for fails the external-provisioner's own check.
package spdk

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/simplyblock/atlas/errs/deferrers"
)

// file name in which volume context is stashed.
const volumeContextFileName = "volume-context.json"

const (
	mib = int64(1024 * 1024)
	gib = mib * 1024
)

func ParseJSONFile(fileName string, result interface{}) error {
	file, err := os.Open(fileName)
	if err != nil {
		return err
	}
	defer deferrers.Close(file)

	bytes, err := io.ReadAll(file)
	if err != nil {
		return err
	}

	return json.Unmarshal(bytes, result)
}

// toGiB rounds up bytes to gigabytes
func toGiB(bytes int64) int64 {
	return (bytes + gib - 1) / gib
}

// alignToGiBBytes rounds bytes up to the next GiB boundary and returns bytes.
func alignToGiBBytes(bytes int64) int64 {
	return toGiB(bytes) * gib
}

// ${env:-def}
func FromEnv(env, def string) string {
	s := os.Getenv(env)
	if s != "" {
		return s
	}
	return def
}

// convertInterfaceToMap converts an interface to a map[string]string
func convertInterfaceToMap(data interface{}) (map[string]string, error) {
	dataMap, ok := data.(map[string]interface{})
	if !ok {
		return nil, errors.New("the data is not a map[string]interface{}")
	}

	strMap := make(map[string]string)
	for key, value := range dataMap {
		if strValue, ok := value.(string); ok {
			strMap[key] = strValue
		} else {
			return nil, fmt.Errorf("the value for key %s is not a string", key)
		}
	}

	return strMap, nil
}

func stashContext(data interface{}, folder, fileName string) error {
	encodedBytes, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("failed to marshall context JSON: %w", err)
	}
	if _, err = os.Stat(folder); os.IsNotExist(err) {
		err = os.MkdirAll(folder, 0o755)
		if err != nil {
			return err
		}
	}
	fPath := filepath.Join(folder, fileName)
	err = os.WriteFile(fPath, encodedBytes, 0o600)
	if err != nil {
		return fmt.Errorf("failed to marshall context JSON at path (%s): %w", fPath, err)
	}
	return nil
}

func lookupContext(folder, fileName string) (interface{}, error) {
	var data interface{}
	fPath := filepath.Join(folder, fileName)
	encodedBytes, err := os.ReadFile(fPath) // #nosec - intended reading from fPath
	if err != nil {
		if !os.IsNotExist(err) {
			return data,
				fmt.Errorf("failed to read stashed context JSON from path (%s): %w", fPath, err)
		}
		return data, errors.New("volume context JSON file not found")
	}
	err = json.Unmarshal(encodedBytes, &data)
	if err != nil {
		return data,
			fmt.Errorf("failed to unmarshall stashed context JSON from path (%s): %w", fPath, err)
	}
	return data, nil
}

func cleanUpContext(folder, fileName string) error {
	fPath := filepath.Join(folder, fileName)
	if err := os.Remove(fPath); err != nil {
		return fmt.Errorf("failed to cleanup volume context stash (%s): %w", fPath, err)
	}
	return nil
}

// stashVolumeContext stashes volume context into the volumeContextFileName at the passed in path, in
// JSON format.
func stashVolumeContext(volumeContext map[string]string, path string) error {
	return stashContext(volumeContext, path, volumeContextFileName)
}

// lookupVolumeContext read and returns stashed volume context at passed in path
func lookupVolumeContext(path string) (map[string]string, error) {
	data, err := lookupContext(path, volumeContextFileName)
	if err != nil {
		return nil, err
	}
	return convertInterfaceToMap(data)
}

// cleanUpVolumeContext cleans up any stashed volume context at passed in path.
func cleanUpVolumeContext(path string) error {
	return cleanUpContext(path, volumeContextFileName)
}
