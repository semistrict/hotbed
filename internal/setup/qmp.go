package setup

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// qmpClient speaks a minimal subset of QEMU Machine Protocol over a Unix socket.
type qmpClient struct {
	conn net.Conn
	r    *bufio.Reader
}

func dialQMP(ctx context.Context, sockPath string) (*qmpClient, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", sockPath)
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	}
	c := &qmpClient{conn: conn, r: bufio.NewReader(conn)}
	// Read QMP greeting.
	if _, err := c.readLine(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("qmp greeting: %w", err)
	}
	// Negotiate capabilities.
	if _, err := c.execute("qmp_capabilities"); err != nil {
		conn.Close()
		return nil, fmt.Errorf("qmp capabilities: %w", err)
	}
	return c, nil
}

func (c *qmpClient) Close() error { return c.conn.Close() }

func (c *qmpClient) readLine() ([]byte, error) {
	line, err := c.r.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	return line, nil
}

type qmpResponse struct {
	Return json.RawMessage `json:"return,omitempty"`
	Error  *struct {
		Class string `json:"class"`
		Desc  string `json:"desc"`
	} `json:"error,omitempty"`
	Event string `json:"event,omitempty"`
}

func (c *qmpClient) execute(command string) (json.RawMessage, error) {
	req, err := json.Marshal(map[string]string{"execute": command})
	if err != nil {
		return nil, err
	}
	req = append(req, '\n')
	if _, err := c.conn.Write(req); err != nil {
		return nil, err
	}
	// Drain any asynchronous QMP events until we receive our command's reply.
	for {
		line, err := c.readLine()
		if err != nil {
			return nil, err
		}
		var resp qmpResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			return nil, fmt.Errorf("parse qmp response: %w", err)
		}
		if resp.Event != "" {
			continue
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("qmp %s: %s: %s", command, resp.Error.Class, resp.Error.Desc)
		}
		return resp.Return, nil
	}
}

// QMPQueryStatus returns the VM's QMP run status (e.g., "running", "shutdown").
func QMPQueryStatus(ctx context.Context, sockPath string) (string, error) {
	c, err := dialQMP(ctx, sockPath)
	if err != nil {
		return "", err
	}
	defer c.Close()
	raw, err := c.execute("query-status")
	if err != nil {
		return "", err
	}
	var st struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return "", err
	}
	return st.Status, nil
}

// QMPPowerdown sends an ACPI power-button event so the Linux guest shuts down
// cleanly and exports its ZFS pool.
func QMPPowerdown(ctx context.Context, sockPath string) error {
	c, err := dialQMP(ctx, sockPath)
	if err != nil {
		return err
	}
	defer c.Close()
	_, err = c.execute("system_powerdown")
	return err
}
