// SPDX-License-Identifier: BUSL-1.1
package scim

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newServer(t *testing.T, token string) *httptest.Server {
	t.Helper()
	s := NewMemoryStore()
	return httptest.NewServer(AuthMiddleware(token, Handler(s)))
}

func do(t *testing.T, method, url, body, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/scim+json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func TestServiceProviderConfigPublic(t *testing.T) {
	srv := newServer(t, "secret")
	defer srv.Close()
	// No token — should still pass per RFC 7644 §4.
	code, body := do(t, "GET", srv.URL+"/scim/v2/ServiceProviderConfig", "", "")
	if code != 200 {
		t.Fatalf("ServiceProviderConfig should be public, got %d", code)
	}
	if !bytes.Contains(body, []byte(`"oauthbearertoken"`)) {
		t.Errorf("missing auth scheme: %s", body)
	}
}

func TestRequiresBearerWhenTokenSet(t *testing.T) {
	srv := newServer(t, "secret")
	defer srv.Close()
	code, _ := do(t, "GET", srv.URL+"/scim/v2/Users", "", "")
	if code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", code)
	}
	code, _ = do(t, "GET", srv.URL+"/scim/v2/Users", "", "wrong")
	if code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with wrong token, got %d", code)
	}
	code, _ = do(t, "GET", srv.URL+"/scim/v2/Users", "", "secret")
	if code != http.StatusOK {
		t.Fatalf("expected 200 with valid token, got %d", code)
	}
}

func TestSCIMDisabledWhenTokenEmpty(t *testing.T) {
	srv := newServer(t, "") // disabled
	defer srv.Close()
	code, _ := do(t, "GET", srv.URL+"/scim/v2/Users", "", "anything")
	if code != http.StatusUnauthorized {
		t.Fatalf("expected 401 when SCIM disabled, got %d", code)
	}
	// Discovery should still work.
	code, _ = do(t, "GET", srv.URL+"/scim/v2/ServiceProviderConfig", "", "")
	if code != http.StatusOK {
		t.Fatalf("discovery should work even when SCIM disabled, got %d", code)
	}
}

func TestUserCreateThenList(t *testing.T) {
	srv := newServer(t, "k")
	defer srv.Close()

	body := `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"userName":"alice@example.com","active":true,"emails":[{"value":"alice@example.com","primary":true}]}`
	code, raw := do(t, "POST", srv.URL+"/scim/v2/Users", body, "k")
	if code != http.StatusCreated {
		t.Fatalf("POST /Users → %d (%s)", code, raw)
	}
	var created User
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" {
		t.Fatal("server should stamp ID")
	}

	code, raw = do(t, "GET", srv.URL+"/scim/v2/Users", "", "k")
	if code != http.StatusOK {
		t.Fatalf("GET /Users → %d", code)
	}
	if !bytes.Contains(raw, []byte("alice@example.com")) {
		t.Fatalf("listing missing user: %s", raw)
	}

	// Filter by userName eq — Okta + Azure AD use this for dedup.
	code, raw = do(t, "GET",
		srv.URL+`/scim/v2/Users?filter=userName%20eq%20"alice@example.com"`, "", "k")
	if code != 200 || !bytes.Contains(raw, []byte("alice@example.com")) {
		t.Fatalf("filter lookup failed: %d %s", code, raw)
	}
}

func TestUserDeprovisionViaPATCHActive(t *testing.T) {
	srv := newServer(t, "k")
	defer srv.Close()
	code, raw := do(t, "POST", srv.URL+"/scim/v2/Users",
		`{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"userName":"bob","active":true}`, "k")
	if code != http.StatusCreated {
		t.Fatal("create failed")
	}
	var created User
	_ = json.Unmarshal(raw, &created)

	// Okta-shape PATCH for deprovisioning.
	patch := `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[{"op":"replace","path":"active","value":false}]}`
	code, _ = do(t, "PATCH", srv.URL+"/scim/v2/Users/"+created.ID, patch, "k")
	if code != http.StatusOK {
		t.Fatalf("PATCH → %d", code)
	}
	code, raw = do(t, "GET", srv.URL+"/scim/v2/Users/"+created.ID, "", "k")
	if !bytes.Contains(raw, []byte(`"active":false`)) {
		t.Fatalf("deprovision didn't flip active: %s", raw)
	}
	_ = code
}

func TestGroupCreateAndDelete(t *testing.T) {
	srv := newServer(t, "k")
	defer srv.Close()

	body := `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:Group"],"displayName":"kubehero-admins","members":[{"value":"u1"}]}`
	code, raw := do(t, "POST", srv.URL+"/scim/v2/Groups", body, "k")
	if code != http.StatusCreated {
		t.Fatalf("POST /Groups → %d", code)
	}
	var g Group
	_ = json.Unmarshal(raw, &g)
	if g.ID == "" {
		t.Fatal("group ID missing")
	}

	code, _ = do(t, "DELETE", srv.URL+"/scim/v2/Groups/"+g.ID, "", "k")
	if code != http.StatusNoContent {
		t.Fatalf("DELETE → %d", code)
	}
}
