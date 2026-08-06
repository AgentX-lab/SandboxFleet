package snapshotter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

type chClient struct {
	apiSocket string
	http      *http.Client
}

type snapshotConfig struct {
	DestinationURL string `json:"destination_url"`
}

func newCHClient(socketPath string) *chClient {
	return &chClient{
		apiSocket: socketPath,
		http: &http.Client{
			Transport: &http.Transport{
				DisableKeepAlives: true,
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socketPath)
				},
			},
		},
	}
}

func (c *chClient) WaitReady(ctx context.Context, deadline time.Duration) error {
	end := time.Now().Add(deadline)
	for {
		if err := c.do(ctx, http.MethodGet, "/api/v1/vmm.ping", nil); err == nil {
			return nil
		}
		if time.Now().After(end) {
			return fmt.Errorf("cloud-hypervisor api %q not ready after %s", c.apiSocket, deadline)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (c *chClient) state(ctx context.Context) (string, error) {
	var info struct {
		State string `json:"state"`
	}
	if err := c.getJSON(ctx, "/api/v1/vm.info", &info); err != nil {
		return "", err
	}
	return info.State, nil
}

func (c *chClient) Pause(ctx context.Context) error {
	if state, err := c.state(ctx); err == nil && state == "Paused" {
		return nil
	}
	return c.do(ctx, http.MethodPut, "/api/v1/vm.pause", nil)
}

func (c *chClient) Resume(ctx context.Context) error {
	return c.do(ctx, http.MethodPut, "/api/v1/vm.resume", nil)
}

func (c *chClient) Snapshot(ctx context.Context, destDir string) error {
	return c.do(ctx, http.MethodPut, "/api/v1/vm.snapshot", snapshotConfig{
		DestinationURL: "file://" + destDir,
	})
}

func (c *chClient) Shutdown(ctx context.Context) error {
	_ = c.do(ctx, http.MethodPut, "/api/v1/vm.shutdown", nil)
	return c.do(ctx, http.MethodPut, "/api/v1/vmm.shutdown", nil)
}

func (c *chClient) do(ctx context.Context, method, path string, body any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, bytes.TrimSpace(msg))
	}
	return nil
}

func (c *chClient) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost"+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("GET %s: status %d: %s", path, resp.StatusCode, bytes.TrimSpace(b))
	}
	return json.Unmarshal(b, out)
}
