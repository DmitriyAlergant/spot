package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestDockerComposePassesCloudflareAccessIDP(t *testing.T) {
	raw, err := os.ReadFile("../docker-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	want := "SPOT_CLOUDFLARE_ACCESS_IDP_ID: ${SPOT_CLOUDFLARE_ACCESS_IDP_ID:-}"
	if !strings.Contains(string(raw), want) {
		t.Fatalf("docker-compose.yml does not pass %s to spot-api", want)
	}
}

func TestSetupCloudflarePagesTokenScript(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is not installed")
	}
	for _, tc := range []struct {
		name        string
		kindArgs    []string
		tokenPath   string
		existingOTP bool
	}{
		{name: "account owned by default", tokenPath: "/accounts/acct/tokens", existingOTP: true},
		{name: "user owned explicitly", kindArgs: []string{"--bootstrap-token-kind", "user"}, tokenPath: "/user/tokens"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sawCreate bool
			var sawCreateOTP bool
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Authorization"); got != "Bearer bootstrap" {
					t.Errorf("Authorization = %q, want bootstrap bearer", got)
				}
				w.Header().Set("Content-Type", "application/json")
				switch r.Method + " " + r.URL.Path {
				case "GET " + tc.tokenPath + "/permission_groups":
					_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": []map[string]string{
						{"id": "pages", "name": "Cloudflare Pages Write"},
						{"id": "dns", "name": "DNS Write"},
						{"id": "access", "name": "Access: Apps and Policies Write"},
						{"id": "zone", "name": "Zone Read"},
					}})
				case "GET /accounts/acct/access/identity_providers":
					result := []map[string]string{{"id": "cloudflare-idp", "type": "cloudflare"}}
					if tc.existingOTP {
						result = append(result, map[string]string{"id": "otp-existing", "type": "onetimepin"})
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": result})
				case "POST /accounts/acct/access/identity_providers":
					sawCreateOTP = true
					_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]string{"id": "otp-created"}})
				case "POST " + tc.tokenPath:
					sawCreate = true
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Errorf("decode body: %v", err)
					}
					if body["name"] != "spot-cloudflare-pages-runtime" {
						t.Errorf("token name = %v", body["name"])
					}
					raw, _ := json.Marshal(body["policies"])
					policyJSON := string(raw)
					for _, want := range []string{`"id":"pages"`, `"id":"dns"`, `"id":"access"`} {
						if !strings.Contains(policyJSON, want) {
							t.Errorf("policies = %s, want %s", policyJSON, want)
						}
					}
					if strings.Contains(policyJSON, `"id":"zone"`) {
						t.Errorf("policies = %s, want no unused Zone Read permission", policyJSON)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]string{"value": "runtime-token"}})
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					http.Error(w, "unexpected request", http.StatusNotFound)
				}
			}))
			defer ts.Close()

			args := []string{"../scripts/setup-cloudflare-pages-token.sh", "--bootstrap-token", "bootstrap"}
			args = append(args, tc.kindArgs...)
			args = append(args, "--account-id", "acct", "--zone-id", "zone-id", "--base-domain", "pages.example.com")
			cmd := exec.Command("sh", args...)
			cmd.Env = append(cmd.Environ(), "CLOUDFLARE_API_BASE_URL="+ts.URL)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("setup script failed: %v\n%s", err, out)
			}
			if !sawCreate {
				t.Fatal("script did not create a token")
			}
			if sawCreateOTP == tc.existingOTP {
				t.Fatalf("create OTP = %v, existing OTP = %v", sawCreateOTP, tc.existingOTP)
			}
			got := string(out)
			for _, want := range []string{
				"SPOT_CLOUDFLARE_API_TOKEN=runtime-token",
				"SPOT_CLOUDFLARE_ACCOUNT_ID=acct",
				"SPOT_CLOUDFLARE_ZONE_ID=zone-id",
				"SPOT_CLOUDFLARE_BASE_DOMAIN=pages.example.com",
				"SPOT_CLOUDFLARE_PROJECT_PREFIX=spot-",
				"SPOT_CLOUDFLARE_ACCESS_IDP_ID=otp-",
			} {
				if !strings.Contains(got, want) {
					t.Fatalf("script output = %q, want %q", got, want)
				}
			}
		})
	}
}

func TestSetupCloudflarePagesTokenScriptRejectsWorkersPermission(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is not installed")
	}
	var sawCreate bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /accounts/acct/tokens/permission_groups":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": []map[string]string{
				{"id": "workers", "name": "Workers Scripts Write"},
				{"id": "dns", "name": "DNS Write"},
				{"id": "zone", "name": "Zone Read"},
			}})
		case "POST /accounts/acct/tokens":
			sawCreate = true
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]string{"value": "bad-token"}})
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer ts.Close()

	cmd := exec.Command("sh", "../scripts/setup-cloudflare-pages-token.sh",
		"--bootstrap-token", "bootstrap", "--account-id", "acct",
		"--zone-id", "zone-id", "--base-domain", "pages.example.com")
	cmd.Env = append(cmd.Environ(), "CLOUDFLARE_API_BASE_URL="+ts.URL)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("setup script succeeded with Workers-only permission: %s", out)
	}
	if sawCreate {
		t.Fatal("script attempted to create a runtime token without Pages Write")
	}
	if !strings.Contains(string(out), "could not find a Cloudflare Pages write permission group") {
		t.Fatalf("setup script output = %q, want Pages permission error", out)
	}
}
