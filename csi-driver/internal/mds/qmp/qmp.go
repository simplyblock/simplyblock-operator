// A QEMU Machine Protocol client for the two things the MDS runner asks of
// QEMU: a clean guest shutdown and whether the guest is still running.
//
// QMP is line-delimited JSON over a socket: QEMU greets, the client negotiates
// capabilities, and then each command gets exactly one return or error, with
// asynchronous events interleaved. The runner needs no events, so they are
// read and dropped. A full client library (neonvm-runner uses go-qemu) brings
// a libvirt dependency for what fits in this file.

package qmp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/simplyblock/atlas/errs/deferrers"
)

// Client is one QMP session. Commands are serialized: QMP answers them in
// order on one stream, so a second command in flight would read the first
// one's reply.
type Client struct {
	mu      sync.Mutex
	conn    net.Conn
	scanner *bufio.Scanner
}

// message is any line QEMU sends: a greeting, an event, a return, or an error.
type message struct {
	QMP    json.RawMessage `json:"QMP"`
	Event  string          `json:"event"`
	Return json.RawMessage `json:"return"`
	Error  *struct {
		Class string `json:"class"`
		Desc  string `json:"desc"`
	} `json:"error"`
}

type command struct {
	Execute   string `json:"execute"`
	Arguments any    `json:"arguments,omitempty"`
}

// Dial connects to the QMP socket at path and negotiates capabilities.
func Dial(ctx context.Context, path string) (*Client, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("connecting to QMP at %s: %w", path, err)
	}
	c := &Client{conn: conn, scanner: bufio.NewScanner(conn)}

	// On failure the session is unusable. The handshake error is returned, and
	// a failing close is logged rather than dropped.
	if err := c.withDeadline(ctx, c.readGreeting); err != nil {
		deferrers.Close(conn)
		return nil, err
	}
	if _, err := c.Execute(ctx, "qmp_capabilities", nil); err != nil {
		deferrers.Close(conn)
		return nil, fmt.Errorf("negotiating QMP capabilities: %w", err)
	}
	return c, nil
}

// Close ends the session.
func (c *Client) Close() error {
	return c.conn.Close()
}

// Execute runs the named command with arguments (nil for none) and returns its
// result as raw JSON, or the error QEMU answered with.
func (c *Client) Execute(ctx context.Context, name string, arguments any) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var result []byte
	err := c.withDeadline(ctx, func() error {
		if err := c.send(name, arguments); err != nil {
			return err
		}
		var err error
		result, err = c.readReply(name)
		return err
	})
	return result, err
}

// SystemPowerdown presses the guest's ACPI power button. The guest shuts down
// on its own, and QEMU exits when it has.
func (c *Client) SystemPowerdown(ctx context.Context) error {
	_, err := c.Execute(ctx, "system_powerdown", nil)
	return err
}

func (c *Client) send(name string, arguments any) error {
	line, err := json.Marshal(command{Execute: name, Arguments: arguments})
	if err != nil {
		return fmt.Errorf("encoding %s: %w", name, err)
	}
	if _, err := c.conn.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("sending %s: %w", name, err)
	}
	return nil
}

func (c *Client) readGreeting() error {
	msg, err := c.read()
	if err != nil {
		return fmt.Errorf("reading QMP greeting: %w", err)
	}
	if msg.QMP == nil {
		return errors.New("QMP peer did not greet")
	}
	return nil
}

// readReply reads up to the reply of the command just sent, dropping the
// events QEMU interleaves.
func (c *Client) readReply(name string) ([]byte, error) {
	for {
		msg, err := c.read()
		if err != nil {
			return nil, fmt.Errorf("reading reply to %s: %w", name, err)
		}
		switch {
		case msg.Event != "":
			continue
		case msg.Error != nil:
			return nil, fmt.Errorf("%s: %s: %s", name, msg.Error.Class, msg.Error.Desc)
		case msg.Return != nil:
			return msg.Return, nil
		}
	}
}

func (c *Client) read() (message, error) {
	if !c.scanner.Scan() {
		if err := c.scanner.Err(); err != nil {
			return message{}, err
		}
		return message{}, errors.New("QMP connection closed")
	}
	var msg message
	if err := json.Unmarshal(c.scanner.Bytes(), &msg); err != nil {
		return message{}, fmt.Errorf("decoding %q: %w", c.scanner.Text(), err)
	}
	return msg, nil
}

// withDeadline runs fn with the connection's deadline set from ctx, so a
// QEMU that stops answering cannot block the caller past its context. A
// context without a deadline clears it.
func (c *Client) withDeadline(ctx context.Context, fn func() error) error {
	deadline, _ := ctx.Deadline()
	if err := c.conn.SetDeadline(deadline); err != nil {
		return fmt.Errorf("setting QMP deadline: %w", err)
	}
	return fn()
}
