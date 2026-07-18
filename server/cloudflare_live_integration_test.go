//go:build integration && cloudflarelive

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCloudflareLivePublishRoundTrip is intentionally excluded from every
// normal test target. It creates a real Pages project, custom domain, and DNS
// record, verifies the deployment URL, then removes all three.
func TestCloudflareLivePublishRoundTrip(t *testing.T) {
	if os.Getenv("SPOT_CLOUDFLARE_LIVE_TEST") != "1" {
		t.Skip("set SPOT_CLOUDFLARE_LIVE_TEST=1 and dedicated SPOT_CLOUDFLARE_* credentials")
	}
	cfg := loadCloudflareConfigFromEnv()
	if !cfg.Enabled() {
		t.Fatalf("live Cloudflare config is %s; missing %s", cfg.Status, strings.Join(cfg.Missing, ", "))
	}
	cfg.ProjectPrefix = envOr("SPOT_CLOUDFLARE_LIVE_TEST_PROJECT_PREFIX", "spot-live-test-")
	site := fmt.Sprintf("live-%x", time.Now().UnixNano())

	db := openTestDB(t)
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO sites (name, owner_email) VALUES (?, 'cloudflare-live-test@spot.local')`, site); err != nil {
		t.Fatal(err)
	}
	repo := NewCloudflarePublicationStore(db)
	publisher := &CloudflarePublisher{cfg: cfg, repo: repo, client: NewCloudflareClient(cfg.APIToken)}
	sites, err := NewLocalSiteStore(filepath.Join(t.TempDir(), "sites"))
	if err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string][]byte{
		"index.html": []byte("<!doctype html><title>Spot Cloudflare live test</title><h1>ok</h1>"),
		"same.html":  []byte("same bytes, distinct MIME identity"),
		"same.txt":   []byte("same bytes, distinct MIME identity"),
	} {
		if err := sites.Put(context.Background(), site, path, contentTypeFor(path, data), data); err != nil {
			t.Fatal(err)
		}
	}
	srv := &Server{sites: sites}
	snap, err := srv.snapshotCloudflareSite(context.Background(), site)
	if err != nil {
		t.Fatal(err)
	}
	if eligibility := checkCloudflareEligibility(snap); !eligibility.Eligible {
		t.Fatalf("live fixture is not eligible: %v", eligibility.Reasons)
	}
	policy := cloudflareAccessPolicy{Mode: cloudflareAccessPublic}
	restrictedEmail := strings.TrimSpace(os.Getenv("SPOT_CLOUDFLARE_LIVE_TEST_EMAIL"))
	if restrictedEmail != "" {
		if !cfg.AccessEnabled() {
			t.Fatal("SPOT_CLOUDFLARE_LIVE_TEST_EMAIL requires SPOT_CLOUDFLARE_ACCESS_IDP_ID")
		}
		var err error
		policy, err = normalizeCloudflareAccessPolicy(cloudflareAccessRestricted, []string{restrictedEmail})
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := publisher.reserve(context.Background(), site, snap, policy); err != nil {
		t.Fatal(err)
	}
	cleaned := false
	t.Cleanup(func() {
		if cleaned {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := cleanupCloudflareLiveFixture(ctx, publisher, repo, site); err != nil {
			t.Errorf("cleanup live Cloudflare publication %s: %v", site, err)
		}
	})

	publishTimeout := cloudflareDomainActiveTimeout + time.Minute
	if policy.Mode == cloudflareAccessRestricted {
		publishTimeout += cloudflareAccessActiveTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), publishTimeout)
	pub, err := publisher.publish(ctx, site, snap, policy)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	if pub.Status != "published" || pub.DeploymentURL == "" {
		t.Fatalf("publication = %+v", pub)
	}
	t.Logf("published live test at https://%s/ (%s)", pub.Hostname, pub.DeploymentURL)
	if restrictedEmail != "" {
		for _, protectedURL := range []string{
			pub.DeploymentURL,
			"https://" + pub.ProjectName + ".pages.dev/",
			"https://" + pub.Hostname + "/",
		} {
			if err := waitForCloudflareAccessChallenge(protectedURL, 3*time.Minute); err != nil {
				t.Fatalf("restricted URL %s was not protected: %v", protectedURL, err)
			}
		}
		publicCtx, publicCancel := context.WithTimeout(context.Background(), cloudflareDomainActiveTimeout+time.Minute)
		pub, err = publisher.publish(publicCtx, site, snap, cloudflareAccessPolicy{Mode: cloudflareAccessPublic})
		publicCancel()
		if err != nil {
			t.Fatalf("return restricted publication to public: %v", err)
		}
	}

	if err := waitForCloudflareResponse(pub.DeploymentURL, "Spot Cloudflare live test", "text/html", 90*time.Second); err != nil {
		t.Fatalf("deployment did not become readable: %v", err)
	}
	deploymentBase := strings.TrimRight(pub.DeploymentURL, "/")
	for _, tc := range []struct {
		path        string
		contentType string
	}{
		{path: "/same.html", contentType: "text/html"},
		{path: "/same.txt", contentType: "text/plain"},
	} {
		if err := waitForCloudflareResponse(deploymentBase+tc.path, "same bytes, distinct MIME identity", tc.contentType, 30*time.Second); err != nil {
			t.Fatalf("asset %s did not preserve MIME identity: %v", tc.path, err)
		}
	}
	if err := waitForCloudflareResponse("https://"+pub.Hostname+"/", "Spot Cloudflare live test", "text/html", 3*time.Minute); err != nil {
		t.Fatalf("custom domain did not become readable: %v", err)
	}
	if restrictedEmail != "" {
		// Exercise the opposite policy transition after proving the public site is
		// readable. Product code must establish Access at the edge before it sends
		// another restricted snapshot to Pages.
		restrictedCtx, restrictedCancel := context.WithTimeout(context.Background(), cloudflareDomainActiveTimeout+cloudflareAccessActiveTimeout+time.Minute)
		pub, err = publisher.publish(restrictedCtx, site, snap, policy)
		restrictedCancel()
		if err != nil {
			t.Fatalf("return public publication to restricted: %v", err)
		}
		for _, protectedURL := range []string{
			pub.DeploymentURL,
			"https://" + pub.ProjectName + ".pages.dev/",
			"https://" + pub.Hostname + "/",
		} {
			if err := waitForCloudflareAccessChallenge(protectedURL, 3*time.Minute); err != nil {
				t.Fatalf("re-restricted URL %s was not protected: %v", protectedURL, err)
			}
		}
	}

	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	err = publisher.unpublish(cleanupCtx, site)
	cleanupCancel()
	if err != nil {
		t.Fatal(err)
	}
	cleaned = true
	if remaining, err := repo.Get(context.Background(), site); err != nil || remaining != nil {
		t.Fatalf("publication state after cleanup = %+v, %v", remaining, err)
	}
}

func waitForCloudflareAccessChallenge(rawURL string, timeout time.Duration) error {
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		requestCtx, requestCancel := context.WithTimeout(context.Background(), 15*time.Second)
		req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, rawURL, nil)
		if err != nil {
			requestCancel()
			return err
		}
		res, requestErr := client.Do(req)
		if requestErr == nil {
			location := res.Header.Get("Location")
			res.Body.Close()
			if res.StatusCode >= 300 && res.StatusCode < 400 &&
				(strings.Contains(location, ".cloudflareaccess.com/") || strings.Contains(location, "/cdn-cgi/access/")) {
				requestCancel()
				return nil
			}
			lastErr = fmt.Errorf("returned %d with Location %q", res.StatusCode, location)
		} else {
			lastErr = requestErr
		}
		requestCancel()
		time.Sleep(2 * time.Second)
	}
	return lastErr
}

// cleanupCloudflareLiveFixture is deliberately more destructive than product
// cleanup: the live test uses a unique project/hostname and must not leak real
// resources when a create succeeds remotely but its response or state write is
// ambiguous. Product code correctly fails closed without ownership proof.
func cleanupCloudflareLiveFixture(ctx context.Context, publisher *CloudflarePublisher, repo *CloudflarePublicationStore, site string) error {
	var cleanupErrs []error
	fallbackSafe := true
	if err := publisher.unpublish(ctx, site); err != nil && !errors.Is(err, ErrSiteNotFound) {
		cleanupErrs = append(cleanupErrs, err)
	}
	projectName := publisher.cfg.ProjectName(site)
	hostname := publisher.cfg.Hostname(site)
	target := projectName + ".pages.dev"
	if record, err := publisher.matchingDNSRecord(ctx, publisher.cfg.ZoneID, hostname, target); err != nil {
		cleanupErrs = append(cleanupErrs, err)
		fallbackSafe = false
	} else if record != nil {
		if err := publisher.client.DeleteDNSRecord(ctx, publisher.cfg.ZoneID, record.ID); err != nil && !errors.Is(err, errCloudflareNotFound) {
			cleanupErrs = append(cleanupErrs, err)
			fallbackSafe = false
		}
	}
	if err := publisher.client.DeleteDomain(ctx, publisher.cfg.AccountID, projectName, hostname); err != nil && !errors.Is(err, errCloudflareNotFound) {
		cleanupErrs = append(cleanupErrs, err)
		fallbackSafe = false
	}
	if err := publisher.client.DeleteProject(ctx, publisher.cfg.AccountID, projectName); err != nil && !errors.Is(err, errCloudflareNotFound) {
		cleanupErrs = append(cleanupErrs, err)
		fallbackSafe = false
	}
	// Normal unpublish keeps Access until Pages is gone. If it failed partway,
	// explicitly remove the still-recorded application before deleting the only
	// durable copy of its ID. This fixture is uniquely named and may therefore
	// clean up even when normal product ownership checks failed closed.
	if pub, err := repo.Get(ctx, site); err != nil {
		cleanupErrs = append(cleanupErrs, err)
		fallbackSafe = false
	} else if pub != nil {
		accountID, _ := publisher.publicationLocation(pub)
		appID := pub.AccessAppID
		if appID == "" && pub.Status == "restricting" {
			includeCustomDomain := pub.DeploymentID != ""
			spec := publisher.accessApplicationSpec(pub, cloudflareAccessPolicy{Mode: cloudflareAccessRestricted}, includeCustomDomain)
			app, findErr := publisher.findAccessApplication(ctx, accountID, spec)
			switch {
			case findErr != nil:
				cleanupErrs = append(cleanupErrs, findErr)
				fallbackSafe = false
			case app == nil:
				cleanupErrs = append(cleanupErrs, errors.New("live cleanup could not reconcile the uncertain Access application"))
				fallbackSafe = false
			default:
				appID = app.ID
			}
		}
		if appID != "" {
			if err := publisher.client.DeleteAccessApplication(ctx, accountID, appID); err != nil && !errors.Is(err, errCloudflareNotFound) {
				cleanupErrs = append(cleanupErrs, err)
				fallbackSafe = false
			}
		}
	}
	if fallbackSafe {
		if err := repo.Delete(ctx, site); err != nil {
			cleanupErrs = append(cleanupErrs, err)
		}
	}
	return errors.Join(cleanupErrs...)
}

func waitForCloudflareResponse(rawURL, wantBody, wantContentType string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		requestCtx, requestCancel := context.WithTimeout(context.Background(), 15*time.Second)
		req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, rawURL, nil)
		if err != nil {
			requestCancel()
			return err
		}
		res, requestErr := http.DefaultClient.Do(req)
		if requestErr == nil {
			body, readErr := io.ReadAll(io.LimitReader(res.Body, 1<<20))
			res.Body.Close()
			contentType := res.Header.Get("Content-Type")
			if readErr == nil && res.StatusCode == http.StatusOK &&
				strings.Contains(string(body), wantBody) && strings.HasPrefix(contentType, wantContentType) {
				requestCancel()
				return nil
			}
			lastErr = fmt.Errorf("returned %d with Content-Type %q: %s", res.StatusCode, contentType, strings.TrimSpace(string(body)))
		} else {
			lastErr = requestErr
		}
		requestCancel()
		time.Sleep(2 * time.Second)
	}
	return lastErr
}
