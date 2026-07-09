// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package client

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kubehero-io/platform/cli/kubehero/internal/config"
)

// capture records what the test server received for one request.
type capture struct {
	method string
	path   string
	header http.Header
	body   string
}

// newCaptureServer returns a server that records the last request and
// responds with the given status and body.
func newCaptureServer(t *testing.T, status int, respBody string) (*httptest.Server, *capture) {
	t.Helper()
	cap := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.method = r.Method
		cap.path = r.URL.Path
		cap.header = r.Header.Clone()
		b, _ := io.ReadAll(r.Body)
		cap.body = string(b)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respBody))
	}))
	t.Cleanup(srv.Close)
	return srv, cap
}

func TestNewTLSAndTimeoutDefaults(t *testing.T) {
	tests := []struct {
		name         string
		insecure     bool
		wantInsecure bool
	}{
		{name: "insecure defaults off", insecure: false, wantInsecure: false},
		{name: "insecure opt-in", insecure: true, wantInsecure: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := New(&config.Config{Endpoint: "https://api.example.com", Insecure: tt.insecure})
			tr, ok := c.http.Transport.(*http.Transport)
			if !ok {
				t.Fatalf("transport is %T, want *http.Transport", c.http.Transport)
			}
			if got := tr.TLSClientConfig.InsecureSkipVerify; got != tt.wantInsecure {
				t.Errorf("InsecureSkipVerify = %v, want %v", got, tt.wantInsecure)
			}
			if c.http.Timeout != 30*time.Second {
				t.Errorf("client timeout = %v, want 30s", c.http.Timeout)
			}
		})
	}
}

func TestRequestShaping(t *testing.T) {
	tests := []struct {
		name     string
		call     func(c *Client) error
		wantPath string
		wantBody string
	}{
		{
			name:     "ListClusters",
			call:     func(c *Client) error { _, err := c.ListClusters(50); return err },
			wantPath: "/kubehero.v1.ControlPlaneService/ListClusters",
			wantBody: `{"pageSize":50}`,
		},
		{
			name: "RegisterCluster",
			call: func(c *Client) error {
				_, err := c.RegisterCluster(&RegisterClusterRequest{
					Name: "prod", Cloud: "aws", Region: "eu-west-1", Slug: "prod-eu", Org: "acme",
				})
				return err
			},
			wantPath: "/kubehero.v1.ControlPlaneService/RegisterCluster",
			wantBody: `{"name":"prod","cloud":"aws","region":"eu-west-1","slug":"prod-eu","org":"acme"}`,
		},
		{
			name: "RegisterCluster omits empty optionals",
			call: func(c *Client) error {
				_, err := c.RegisterCluster(&RegisterClusterRequest{Name: "dev", Cloud: "gcp", Region: "us-east1"})
				return err
			},
			wantPath: "/kubehero.v1.ControlPlaneService/RegisterCluster",
			wantBody: `{"name":"dev","cloud":"gcp","region":"us-east1"}`,
		},
		{
			name:     "HealthCheck",
			call:     func(c *Client) error { _, err := c.HealthCheck(); return err },
			wantPath: "/kubehero.v1.ControlPlaneService/HealthCheck",
			wantBody: `null`,
		},
		{
			name:     "Quote",
			call:     func(c *Client) error { _, err := c.Quote("aws", "m5.large", "eu-west-1", "spot"); return err },
			wantPath: "/kubehero.v1.PricingService/Quote",
			wantBody: `{"cloud":"aws","lifecycle":"spot","region":"eu-west-1","sku":"m5.large"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, cap := newCaptureServer(t, http.StatusOK, `{}`)
			c := New(&config.Config{Endpoint: srv.URL, Token: "tok-123"})
			if err := tt.call(c); err != nil {
				t.Fatal(err)
			}
			if cap.method != http.MethodPost {
				t.Errorf("method = %q, want POST", cap.method)
			}
			if cap.path != tt.wantPath {
				t.Errorf("path = %q, want %q", cap.path, tt.wantPath)
			}
			if got := cap.header.Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
			if got := cap.header.Get("Connect-Protocol-Version"); got != "1" {
				t.Errorf("Connect-Protocol-Version = %q, want 1", got)
			}
			if got := cap.header.Get("Authorization"); got != "Bearer tok-123" {
				t.Errorf("Authorization = %q, want Bearer tok-123", got)
			}
			if !jsonEqual(t, cap.body, tt.wantBody) {
				t.Errorf("body = %s, want %s", cap.body, tt.wantBody)
			}
		})
	}
}

func jsonEqual(t *testing.T, a, b string) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal([]byte(a), &av); err != nil {
		t.Fatalf("invalid JSON %q: %v", a, err)
	}
	if err := json.Unmarshal([]byte(b), &bv); err != nil {
		t.Fatalf("invalid JSON %q: %v", b, err)
	}
	aj, _ := json.Marshal(av)
	bj, _ := json.Marshal(bv)
	return string(aj) == string(bj)
}

func TestAuthHeaderOmittedWithoutToken(t *testing.T) {
	srv, cap := newCaptureServer(t, http.StatusOK, `{"status":"ok"}`)
	c := New(&config.Config{Endpoint: srv.URL})
	if _, err := c.HealthCheck(); err != nil {
		t.Fatal(err)
	}
	if _, present := cap.header["Authorization"]; present {
		t.Errorf("Authorization header sent without token: %q", cap.header.Get("Authorization"))
	}
}

func TestEndpointTrailingSlash(t *testing.T) {
	srv, cap := newCaptureServer(t, http.StatusOK, `{}`)
	c := New(&config.Config{Endpoint: srv.URL + "///"})
	if _, err := c.HealthCheck(); err != nil {
		t.Fatal(err)
	}
	want := "/kubehero.v1.ControlPlaneService/HealthCheck"
	if cap.path != want {
		t.Errorf("path = %q, want %q (trailing slashes should collapse)", cap.path, want)
	}
}

func TestResponseDecoding(t *testing.T) {
	t.Run("ListClusters", func(t *testing.T) {
		srv, _ := newCaptureServer(t, http.StatusOK,
			`{"clusters":[{"id":"c-1","name":"prod","cloud":"aws","region":"eu-west-1","nodes":12}],"nextPageToken":"p2"}`)
		c := New(&config.Config{Endpoint: srv.URL})
		got, err := c.ListClusters(10)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Clusters) != 1 {
			t.Fatalf("clusters = %d, want 1", len(got.Clusters))
		}
		cl := got.Clusters[0]
		if cl.ID != "c-1" || cl.Name != "prod" || cl.Cloud != "aws" || cl.Region != "eu-west-1" || cl.Nodes != 12 {
			t.Errorf("cluster = %+v", cl)
		}
		if got.NextPage != "p2" {
			t.Errorf("nextPage = %q, want p2", got.NextPage)
		}
	})

	t.Run("RegisterCluster", func(t *testing.T) {
		srv, _ := newCaptureServer(t, http.StatusOK,
			`{"cluster":{"id":"c-9","name":"dev"},"token":"agent-tok","helmInstall":"helm install kubehero ..."}`)
		c := New(&config.Config{Endpoint: srv.URL})
		got, err := c.RegisterCluster(&RegisterClusterRequest{Name: "dev", Cloud: "gcp", Region: "us-east1"})
		if err != nil {
			t.Fatal(err)
		}
		if got.Cluster.ID != "c-9" || got.Token != "agent-tok" || !strings.HasPrefix(got.HelmInstall, "helm install") {
			t.Errorf("response = %+v", got)
		}
	})

	t.Run("Quote", func(t *testing.T) {
		srv, _ := newCaptureServer(t, http.StatusOK, `{"pricePerHour":0.0416,"currency":"USD"}`)
		c := New(&config.Config{Endpoint: srv.URL})
		got, err := c.Quote("aws", "t3.medium", "us-east-1", "ondemand")
		if err != nil {
			t.Fatal(err)
		}
		if got.PricePerHour != 0.0416 || got.Currency != "USD" {
			t.Errorf("quote = %+v", got)
		}
	})
}

func TestErrorResponses(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		wantSubs []string
	}{
		{
			name:     "401 unauthenticated",
			status:   http.StatusUnauthorized,
			body:     `{"code":"unauthenticated","message":"invalid token"}`,
			wantSubs: []string{"HealthCheck", "401", "invalid token"},
		},
		{
			name:     "500 with plain body",
			status:   http.StatusInternalServerError,
			body:     "boom",
			wantSubs: []string{"HealthCheck", "500", "boom"},
		},
		{
			name:     "404 empty body",
			status:   http.StatusNotFound,
			body:     "",
			wantSubs: []string{"HealthCheck", "404"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := newCaptureServer(t, tt.status, tt.body)
			c := New(&config.Config{Endpoint: srv.URL})
			_, err := c.HealthCheck()
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			for _, sub := range tt.wantSubs {
				if !strings.Contains(err.Error(), sub) {
					t.Errorf("error %q missing %q", err.Error(), sub)
				}
			}
		})
	}
}

func TestNoEndpointConfigured(t *testing.T) {
	c := New(&config.Config{})
	_, err := c.HealthCheck()
	if err == nil {
		t.Fatal("expected error with empty endpoint")
	}
	if !strings.Contains(err.Error(), "no endpoint configured") {
		t.Errorf("error = %q, want mention of missing endpoint", err.Error())
	}
}

func TestMalformedResponseBody(t *testing.T) {
	srv, _ := newCaptureServer(t, http.StatusOK, `{not json`)
	c := New(&config.Config{Endpoint: srv.URL})
	if _, err := c.HealthCheck(); err == nil {
		t.Fatal("expected JSON decode error, got nil")
	}
}

func TestClientTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)

	c := New(&config.Config{Endpoint: srv.URL})
	c.http.Timeout = 50 * time.Millisecond // shrink the 30s default so the test is fast

	start := time.Now()
	_, err := c.HealthCheck()
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "rpc HealthCheck") {
		t.Errorf("error %q should be wrapped with the RPC method", err.Error())
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("call took %v, timeout did not fire", elapsed)
	}
}
