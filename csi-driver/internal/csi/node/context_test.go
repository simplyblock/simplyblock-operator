/*
Copyright (c) Arm Limited and Contributors.

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

// blackbox test of util package
package node

import (
	"os"
	"testing"
)

func TestVolumeContext(t *testing.T) {
	volumeContextFileName := "volumeContext.json"

	dir, err := os.MkdirTemp("", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	volumeContext := map[string]string{
		"key1": "value1",
		"key2": "value2",
	}

	err = stashVolumeContext(volumeContext, dir)
	if err != nil {
		t.Fatalf("stashVolumeContext returned error: %v", err)
	}

	returnedContext, err := lookupVolumeContext(dir)
	if err != nil {
		t.Fatalf("lookupVolumeContext returned error: %v", err)
	}

	if volumeContext["key1"] != returnedContext["key1"] || volumeContext["key2"] != returnedContext["key2"] {
		t.Fatalf("lookupVolumeContext returned unexpected value: got %v, want %v", returnedContext, volumeContext)
	}

	err = cleanUpVolumeContext(dir)
	if err != nil {
		t.Fatalf("cleanUpVolumeContext returned error: %v", err)
	}

	_, err = os.Stat(dir + "/" + volumeContextFileName)
	if !os.IsNotExist(err) {
		t.Fatalf("cleanUpVolumeContext failed to cleanup volume context stash")
	}
}
