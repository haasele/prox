package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/haasele/prox/internal/config"
)

type Client struct {
	Socket     string
	HTTP       *http.Client
	StreamHTTP *http.Client
}

func NewClient(socket string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
		DisableCompression:  true,
		MaxIdleConns:        4,
		MaxIdleConnsPerHost: 4,
	}
	return &Client{
		Socket:     socket,
		HTTP:       &http.Client{Transport: tr, Timeout: 30 * time.Second},
		StreamHTTP: &http.Client{Transport: tr},
	}
}

func clientConfig() (*config.Config, *Client, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	return cfg, NewClient(cfg.APISocket), nil
}

func (c *Client) Do(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://prox"+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if isDaemonDown(err) {
			return fmt.Errorf("prox daemon is not running at %s.\nStart it with: proxd   (or: systemctl --user start prox)", c.Socket)
		}
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		var ej struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &ej)
		if ej.Error != "" {
			return fmt.Errorf("%s", ej.Error)
		}
		return fmt.Errorf("daemon returned %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

func (c *Client) Get(ctx context.Context, path string, out any) error {
	return c.Do(ctx, http.MethodGet, path, nil, out)
}

func (c *Client) Post(ctx context.Context, path string, body any, out any) error {
	return c.Do(ctx, http.MethodPost, path, body, out)
}

func (c *Client) Delete(ctx context.Context, path string, out any) error {
	return c.Do(ctx, http.MethodDelete, path, nil, out)
}

func (c *Client) Stream(ctx context.Context, path string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://prox"+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.StreamHTTP.Do(req)
	if err != nil {
		if isDaemonDown(err) {
			return nil, fmt.Errorf("prox daemon is not running at %s.\nStart it with: proxd   (or: systemctl --user start prox)", c.Socket)
		}
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("daemon returned %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return resp.Body, nil
}

func (c *Client) GetBytes(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://prox"+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if isDaemonDown(err) {
			return nil, fmt.Errorf("prox daemon is not running at %s", c.Socket)
		}
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("daemon returned %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return b, nil
}

func (c *Client) PostRaw(ctx context.Context, path, contentType string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://prox"+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if isDaemonDown(err) {
			return fmt.Errorf("prox daemon is not running at %s.\nStart it with: proxd   (or: systemctl --user start prox)", c.Socket)
		}
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		var ej struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &ej)
		if ej.Error != "" {
			return fmt.Errorf("%s", ej.Error)
		}
		return fmt.Errorf("daemon returned %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func isDaemonDown(err error) bool {
	if os.IsNotExist(err) {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "connection refused") ||
		strings.Contains(s, "no such file or directory") ||
		strings.Contains(s, "connect: no such file")
}
