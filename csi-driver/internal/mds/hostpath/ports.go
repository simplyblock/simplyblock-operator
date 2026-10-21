// The storage listener ports the proxy may dial on other nodes, as the operator
// writes them: one file of ports separated by whitespace, mounted from a
// ConfigMap.

package hostpath

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ReadPorts reads a ports file. A missing file allows no port.
func ReadPorts(path string) ([]uint16, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ParsePorts(string(b))
}

// ParsePorts parses ports separated by whitespace.
func ParsePorts(s string) ([]uint16, error) {
	var ports []uint16
	for _, w := range strings.Fields(s) {
		n, err := strconv.ParseUint(w, 10, 16)
		if err != nil || n == 0 {
			return nil, fmt.Errorf("port %q in the ports file is not one", w)
		}
		ports = append(ports, uint16(n))
	}
	return ports, nil
}
