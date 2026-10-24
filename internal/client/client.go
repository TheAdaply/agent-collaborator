// Package client talks to the local Agent Collaborator daemon over its control socket.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/TheAdaply/agent-collaborator/internal/command"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Client calls the daemon's control API.
type Client struct {
	socket string
	hc     *http.Client
}

// ErrNoDaemon means nothing is listening on the control socket.
var ErrNoDaemon = errors.New(command.Text("Agent Collaborator daemon is not running (start it: @COMMAND@ up)"))

// New returns a client for the daemon at socket.
func New(socket string) *Client {
	return &Client{socket: socket, hc: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}}
}

// Do sends a request and decodes a JSON answer into out (which may be nil).
func (c *Client) Do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://agent-collaborator"+path, body)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			return ErrNoDaemon
		}
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s", strings.TrimSpace(string(b)))
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

// Wait blocks until the reply to ask id arrives, up to timeout.
func (c *Client) Wait(ctx context.Context, id string, timeout time.Duration, out any) error {
	ctx, cancel := context.WithTimeout(ctx, timeout+5*time.Second)
	defer cancel()
	return c.Do(ctx, "GET", fmt.Sprintf("/v1/wait/%s?timeout=%d", id, int(timeout.Seconds())), nil, out)
}
