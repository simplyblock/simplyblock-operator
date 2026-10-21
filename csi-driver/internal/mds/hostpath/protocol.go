// The one-line protocol between the forwarder and the proxy: a request line,
// an answer line, and for CONNECT the stream afterward (design-pnfs-mds-vm.md
// §8.5).
//
// Text rather than a binary header so a stuck exchange can be read in a packet
// capture, and versioned by its first word so a runner and a proxy from
// different releases fail on the first line rather than misreading a stream.

package hostpath

import (
	"bufio"
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

// Magic opens every request and names the protocol's version.
const Magic = "SBMDS1"

// Verb is what a request asks the proxy to do.
type Verb string

const (
	// VerbConnect asks the proxy to connect to an address of its node and
	// carry the stream there.
	VerbConnect Verb = "CONNECT"
	// VerbAddrs asks the proxy for its node's addresses.
	VerbAddrs Verb = "ADDRS"
)

// answerOK and answerErr open the proxy's answer line.
const (
	answerOK  = "OK"
	answerErr = "ERR"
)

// maxLine bounds a request or answer line, so a peer that never sends a
// newline costs the reader a fixed amount of memory. An ADDRS answer for a
// node with a few dozen addresses fits.
const maxLine = 1024

// Request is one parsed request line.
type Request struct {
	Verb Verb
	// Target is CONNECT's destination, and unset for ADDRS.
	Target netip.AddrPort
}

// ConnectRequest is the request line asking for a connection to target.
func ConnectRequest(target netip.AddrPort) string {
	return fmt.Sprintf("%s %s %s\n", Magic, VerbConnect, target)
}

// AddrsRequest is the request line asking for the node's addresses.
func AddrsRequest() string {
	return fmt.Sprintf("%s %s\n", Magic, VerbAddrs)
}

// ParseRequest parses a request line without its newline.
func ParseRequest(line string) (Request, error) {
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[0] != Magic {
		return Request{}, fmt.Errorf("not a %s request: %q", Magic, line)
	}
	switch Verb(fields[1]) {
	case VerbAddrs:
		if len(fields) != 2 {
			return Request{}, fmt.Errorf("%s takes no argument: %q", VerbAddrs, line)
		}
		return Request{Verb: VerbAddrs}, nil
	case VerbConnect:
		if len(fields) != 3 {
			return Request{}, fmt.Errorf("%s takes one address and port: %q", VerbConnect, line)
		}
		target, err := netip.ParseAddrPort(fields[2])
		if err != nil {
			return Request{}, fmt.Errorf("%s target %q: %w", VerbConnect, fields[2], err)
		}
		if !target.Addr().Is4() {
			return Request{}, fmt.Errorf("%s target %s is not an IPv4 address", VerbConnect, target)
		}
		return Request{Verb: VerbConnect, Target: target}, nil
	default:
		return Request{}, fmt.Errorf("unknown request %q", fields[1])
	}
}

// ParseAddrsReply parses the answer to ADDRS, without its newline.
func ParseAddrsReply(line string) ([]netip.Addr, error) {
	fields := strings.Fields(line)
	if len(fields) == 0 || fields[0] != answerOK {
		return nil, fmt.Errorf("the proxy refused %s: %q", VerbAddrs, line)
	}
	addrs := make([]netip.Addr, 0, len(fields)-1)
	for _, f := range fields[1:] {
		addr, err := netip.ParseAddr(f)
		if err != nil {
			return nil, fmt.Errorf("the proxy answered %s with %q: %w", VerbAddrs, f, err)
		}
		addrs = append(addrs, addr)
	}
	return addrs, nil
}

// readLine reads one line and returns it without its newline.
func readLine(r *bufio.Reader) (string, error) {
	var b strings.Builder
	for {
		chunk, isPrefix, err := r.ReadLine()
		if err != nil {
			return "", err
		}
		if b.Len()+len(chunk) > maxLine {
			return "", errors.New("line too long")
		}
		b.Write(chunk)
		if !isPrefix {
			return b.String(), nil
		}
	}
}

// okLine and errLine are the proxy's answer lines.
func okLine(rest ...string) string {
	return strings.Join(append([]string{answerOK}, rest...), " ") + "\n"
}

func errLine(err error) string {
	// The reason is one line, so a newline in an error cannot end the answer
	// early and be read as the stream.
	reason := strings.ReplaceAll(err.Error(), "\n", " ")
	return answerErr + " " + reason + "\n"
}
