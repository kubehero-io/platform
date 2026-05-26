// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

// Package client is a tiny Connect-RPC client over plain HTTP for the CLI.
// Mirrors the schema in apps/dashboard/lib/api/client.ts.

package client

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kubehero-io/platform/cli/kubehero/internal/config"
)

type Client struct {
	cfg  *config.Config
	http *http.Client
}

func New(cfg *config.Config) *Client {
	tr := &http.Transport{
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: cfg.Insecure}, //nolint:gosec
		MaxIdleConns:        20,
		MaxConnsPerHost:     10,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	return &Client{
		cfg: cfg,
		http: &http.Client{
			Transport: tr,
			Timeout:   30 * time.Second,
		},
	}
}

// Cluster mirrors the Connect proto.
type Cluster struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Cloud  string `json:"cloud"`
	Region string `json:"region"`
	Nodes  int32  `json:"nodes"`
}

type ListClustersResponse struct {
	Clusters []Cluster `json:"clusters"`
	NextPage string    `json:"nextPageToken"`
}

type RegisterClusterRequest struct {
	Name   string `json:"name"`
	Cloud  string `json:"cloud"`
	Region string `json:"region"`
	Slug   string `json:"slug,omitempty"`
	Org    string `json:"org,omitempty"`
}

type RegisterClusterResponse struct {
	Cluster     Cluster `json:"cluster"`
	Token       string  `json:"token"`
	HelmInstall string  `json:"helmInstall"`
}

type HealthCheckResponse struct {
	Status  string `json:"status"`
	Version string `json:"version"`
}

type QuoteResponse struct {
	PricePerHour float64 `json:"pricePerHour"`
	Currency     string  `json:"currency"`
}

func (c *Client) ListClusters(pageSize int32) (*ListClustersResponse, error) {
	var out ListClustersResponse
	err := c.call("kubehero.v1.ControlPlaneService", "ListClusters",
		map[string]any{"pageSize": pageSize}, &out)
	return &out, err
}

func (c *Client) RegisterCluster(req *RegisterClusterRequest) (*RegisterClusterResponse, error) {
	var out RegisterClusterResponse
	err := c.call("kubehero.v1.ControlPlaneService", "RegisterCluster", req, &out)
	return &out, err
}

func (c *Client) HealthCheck() (*HealthCheckResponse, error) {
	var out HealthCheckResponse
	err := c.call("kubehero.v1.ControlPlaneService", "HealthCheck", nil, &out)
	return &out, err
}

func (c *Client) Quote(cloud, sku, region, lifecycle string) (*QuoteResponse, error) {
	var out QuoteResponse
	err := c.call("kubehero.v1.PricingService", "Quote",
		map[string]any{"cloud": cloud, "sku": sku, "region": region, "lifecycle": lifecycle}, &out)
	return &out, err
}

func (c *Client) call(service, method string, req, out any) error {
	if c.cfg.Endpoint == "" {
		return errors.New("no endpoint configured · run: kubehero auth login --endpoint=...")
	}
	body, _ := json.Marshal(req)
	url := strings.TrimRight(c.cfg.Endpoint, "/") + "/" + service + "/" + method
	hreq, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Connect-Protocol-Version", "1")
	if c.cfg.Token != "" {
		hreq.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	}

	res, err := c.http.Do(hreq)
	if err != nil {
		return fmt.Errorf("rpc %s: %w", method, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 400 {
		return fmt.Errorf("rpc %s: %s · %s", method, res.Status, strings.TrimSpace(string(raw)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}
