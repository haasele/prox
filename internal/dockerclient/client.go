package dockerclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	Socket string
	HTTP   *http.Client
}

func New(socket string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
		DisableCompression:  true,
		MaxIdleConns:        16,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
	}
	return &Client{Socket: socket, HTTP: &http.Client{Transport: tr}}
}

type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("docker API %d: %s", e.Status, e.Message)
}

func (c *Client) Do(ctx context.Context, method, path string, query url.Values, body []byte, headers http.Header) (*http.Response, error) {
	u := "http://docker" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, err
	}
	req.Host = "docker"
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if body != nil {
		req.ContentLength = int64(len(body))
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach Docker daemon at %s: %w", c.Socket, err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var ej struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(b, &ej)
		msg := ej.Message
		if msg == "" {
			msg = strings.TrimSpace(string(b))
		}
		return nil, &APIError{Status: resp.StatusCode, Message: msg}
	}
	return resp, nil
}

func (c *Client) JSON(ctx context.Context, method, path string, query url.Values, in, out any) error {
	var body []byte
	var err error
	if in != nil {
		body, err = json.Marshal(in)
		if err != nil {
			return err
		}
	}
	resp, err := c.Do(ctx, method, path, query, body, http.Header{"Content-Type": []string{"application/json"}})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

type ImageInfo struct {
	ID     string `json:"Id"`
	Config struct {
		ExposedPorts map[string]struct{} `json:"ExposedPorts"`
	} `json:"Config"`
}

func (c *Client) InspectImage(ctx context.Context, name string) (*ImageInfo, error) {
	var out ImageInfo
	if err := c.JSON(ctx, http.MethodGet, "/images/"+name+"/json", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

type Info struct {
	ID              string   `json:"ID"`
	Name            string   `json:"Name"`
	ServerVersion   string   `json:"ServerVersion"`
	OperatingSystem string   `json:"OperatingSystem"`
	OSType          string   `json:"OSType"`
	Architecture    string   `json:"Architecture"`
	SecurityOptions []string `json:"SecurityOptions"`
	DockerRootDir   string   `json:"DockerRootDir"`
}

func (c *Client) Info(ctx context.Context) (*Info, error) {
	var out Info
	if err := c.JSON(ctx, http.MethodGet, "/info", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Ping(ctx context.Context) error {
	resp, err := c.Do(ctx, http.MethodGet, "/_ping", nil, nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

type PortBinding struct {
	HostIP   string `json:"HostIp,omitempty"`
	HostPort string `json:"HostPort,omitempty"`
}

type Port struct {
	IP          string `json:"IP,omitempty"`
	PrivatePort int    `json:"PrivatePort"`
	PublicPort  int    `json:"PublicPort,omitempty"`
	Type        string `json:"Type"`
}

type ContainerSummary struct {
	ID         string            `json:"Id"`
	Names      []string          `json:"Names"`
	Image      string            `json:"Image"`
	ImageID    string            `json:"ImageID"`
	Command    string            `json:"Command"`
	Created    int64             `json:"Created"`
	State      string            `json:"State"`
	Status     string            `json:"Status"`
	Labels     map[string]string `json:"Labels"`
	Ports      []Port            `json:"Ports"`
	HostConfig struct {
		NetworkMode string `json:"NetworkMode"`
	} `json:"HostConfig"`
}

type NetEndpoint struct {
	IPAddress         string   `json:"IPAddress"`
	GlobalIPv6Address string   `json:"GlobalIPv6Address"`
	Gateway           string   `json:"Gateway"`
	NetworkID         string   `json:"NetworkID"`
	Aliases           []string `json:"Aliases"`
}

type Health struct {
	Status string `json:"Status"`
}

type Container struct {
	ID      string `json:"Id"`
	Name    string `json:"Name"`
	Created string `json:"Created"`
	Image   string `json:"Image"`
	Config  struct {
		Hostname     string              `json:"Hostname"`
		Image        string              `json:"Image"`
		ExposedPorts map[string]struct{} `json:"ExposedPorts"`
		Labels       map[string]string   `json:"Labels"`
		Cmd          []string            `json:"Cmd"`
		Env          []string            `json:"Env"`
	} `json:"Config"`
	HostConfig struct {
		NetworkMode     string                   `json:"NetworkMode"`
		PortBindings    map[string][]PortBinding `json:"PortBindings"`
		PublishAllPorts bool                     `json:"PublishAllPorts"`
		ExtraHosts      []string                 `json:"ExtraHosts"`
	} `json:"HostConfig"`
	State struct {
		Status   string  `json:"Status"`
		Running  bool    `json:"Running"`
		Paused   bool    `json:"Paused"`
		Dead     bool    `json:"Dead"`
		Pid      int     `json:"Pid"`
		ExitCode int     `json:"ExitCode"`
		Health   *Health `json:"Health,omitempty"`
	} `json:"State"`
	NetworkSettings struct {
		Networks map[string]*NetEndpoint  `json:"Networks"`
		Ports    map[string][]PortBinding `json:"Ports"`
	} `json:"NetworkSettings"`
}

func (c *Client) ListContainers(ctx context.Context, all bool) ([]ContainerSummary, error) {
	q := url.Values{}
	if all {
		q.Set("all", "1")
	}
	var out []ContainerSummary
	err := c.JSON(ctx, http.MethodGet, "/containers/json", q, nil, &out)
	return out, err
}

func (c *Client) InspectContainer(ctx context.Context, id string) (*Container, error) {
	var out Container
	if err := c.JSON(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/json", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

type Event struct {
	Type   string `json:"Type"`
	Action string `json:"Action"`
	Status string `json:"status"`
	Actor  struct {
		ID         string            `json:"ID"`
		Attributes map[string]string `json:"Attributes"`
	} `json:"Actor"`
	Time     int64 `json:"time"`
	TimeNano int64 `json:"timeNano"`
}

func (c *Client) Events(ctx context.Context, since int64) (*http.Response, error) {
	q := url.Values{}
	if since > 0 {
		q.Set("since", strconv.FormatInt(since, 10))
	}
	q.Set("filters", `{"type":["container"],"event":["start","die","destroy","health_status","rename"]}`)
	return c.Do(ctx, http.MethodGet, "/events", q, nil, nil)
}

func DecodeEvents(r io.Reader, fn func(Event) error) error {
	dec := json.NewDecoder(bufio.NewReader(r))
	for {
		var ev Event
		if err := dec.Decode(&ev); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if ev.Action == "" {
			ev.Action = ev.Status
		}
		if err := fn(ev); err != nil {
			return err
		}
	}
}

type Network struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
	Driver string `json:"Driver"`
	IPAM   struct {
		Config []struct {
			Subnet  string `json:"Subnet"`
			Gateway string `json:"Gateway"`
		} `json:"Config"`
	} `json:"IPAM"`
	Containers map[string]struct {
		Name        string `json:"Name"`
		IPv4Address string `json:"IPv4Address"`
	} `json:"Containers"`
	Labels map[string]string `json:"Labels"`
}

func (c *Client) InspectNetwork(ctx context.Context, name string) (*Network, error) {
	var out Network
	if err := c.JSON(ctx, http.MethodGet, "/networks/"+url.PathEscape(name), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) CreateNetwork(ctx context.Context, name string, labels map[string]string) (string, error) {
	body := map[string]any{
		"Name":           name,
		"Driver":         "bridge",
		"Labels":         labels,
		"CheckDuplicate": true,
	}
	var out struct {
		ID      string `json:"Id"`
		Warning string `json:"Warning"`
	}
	if err := c.JSON(ctx, http.MethodPost, "/networks/create", nil, body, &out); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && strings.Contains(apiErr.Message, "already exists") {
			return "", nil
		}
		return "", err
	}
	return out.ID, nil
}

func (c *Client) ConnectNetwork(ctx context.Context, network, containerID string) error {
	body := map[string]any{"Container": containerID}
	err := c.JSON(ctx, http.MethodPost, "/networks/"+url.PathEscape(network)+"/connect", nil, body, nil)
	if err != nil && strings.Contains(err.Error(), "already exists") {
		return nil
	}
	return err
}

func (c *Client) DisconnectNetwork(ctx context.Context, network, containerID string, force bool) error {
	body := map[string]any{"Container": containerID, "Force": force}
	return c.JSON(ctx, http.MethodPost, "/networks/"+url.PathEscape(network)+"/disconnect", nil, body, nil)
}

func (c *Client) ContainerLogs(ctx context.Context, id string, follow bool, tail string) (*http.Response, error) {
	q := url.Values{}
	if follow {
		q.Set("follow", "1")
	}
	if tail != "" {
		q.Set("tail", tail)
	}
	q.Set("stdout", "1")
	q.Set("stderr", "1")
	return c.Do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/logs", q, nil, nil)
}
