// The node's default host NQN: the one nvme-cli presents on a connect that
// names none, read from the file or rebuilt from the persisted hostid.

package nqn

import (
	"errors"
	"testing"
)

const testHostID = "0b5c3e9a-6f1d-4d2a-9c7e-2f8b1a4d6e30"

func files(m map[string]string) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) {
		if v, ok := m[path]; ok {
			return []byte(v), nil
		}
		return nil, errors.New("no such file")
	}
}

func TestNVMeHost(t *testing.T) {
	want := "nqn.2014-08.org.nvmexpress:uuid:" + testHostID
	if got := NVMeHost(testHostID); got != want {
		t.Errorf("NVMeHost = %q, want %q", got, want)
	}
	if id, ok := HostUUID(NVMeHost(testHostID)); !ok || id != testHostID {
		t.Errorf("HostUUID(NVMeHost) = %q, %v", id, ok)
	}
}

func TestDefaultHostReadsTheHostNQNFile(t *testing.T) {
	read := files(map[string]string{
		DefaultHostNQNFile:  "nqn.2014-08.org.nvmexpress:uuid:1111\n",
		PersistedHostIDFile: "2222\n",
	})
	got, err := DefaultHost(read)
	if err != nil {
		t.Fatalf("DefaultHost: %v", err)
	}
	if got != "nqn.2014-08.org.nvmexpress:uuid:1111" {
		t.Errorf("DefaultHost = %q", got)
	}
}

// The postStart hook writes /etc/nvme/hostnqn concurrently with the plugin's
// start; the persisted hostid it is built from gives the same NQN.
func TestDefaultHostFallsBackToTheHostID(t *testing.T) {
	got, err := DefaultHost(files(map[string]string{PersistedHostIDFile: "2222\n"}))
	if err != nil {
		t.Fatalf("DefaultHost: %v", err)
	}
	if got != "nqn.2014-08.org.nvmexpress:uuid:2222" {
		t.Errorf("DefaultHost = %q", got)
	}
}

// A half-written or empty hostnqn file is not an NQN; the hostid still is.
func TestDefaultHostIgnoresAHostNQNFileThatIsNoNQN(t *testing.T) {
	read := files(map[string]string{DefaultHostNQNFile: "\n", PersistedHostIDFile: "2222"})
	got, err := DefaultHost(read)
	if err != nil {
		t.Fatalf("DefaultHost: %v", err)
	}
	if got != "nqn.2014-08.org.nvmexpress:uuid:2222" {
		t.Errorf("DefaultHost = %q", got)
	}
}

// Without either file there is nothing true to return: an invented NQN would
// name a host identity nobody presents.
func TestDefaultHostFailsWithoutAHostID(t *testing.T) {
	if got, err := DefaultHost(files(nil)); err == nil {
		t.Fatalf("DefaultHost = %q, want an error", got)
	}
	if got, err := DefaultHost(files(map[string]string{PersistedHostIDFile: " \n"})); err == nil {
		t.Fatalf("DefaultHost with an empty hostid = %q, want an error", got)
	}
}
