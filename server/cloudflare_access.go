package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	cloudflareAccessPublic        = "public"
	cloudflareAccessRestricted    = "restricted"
	maxCloudflareAccessEmails     = 100
	maxCloudflarePublishBody      = 64 << 10
	cloudflareAccessActiveTimeout = 2 * time.Minute
)

var (
	errCloudflareAccessCreateUncertain = errors.New("Cloudflare Access application creation is uncertain")
	errCloudflareAccessNotUncertain    = errors.New("Cloudflare Access application creation is not awaiting manual resolution")
)

type cloudflareAccessPolicy struct {
	Mode   string
	Emails []string
}

type cloudflarePublishRequest struct {
	Visibility string   `json:"visibility"`
	Emails     []string `json:"emails"`
}

func cloudflarePolicyArg(requested []cloudflareAccessPolicy) cloudflareAccessPolicy {
	if len(requested) == 0 {
		return cloudflareAccessPolicy{Mode: cloudflareAccessPublic}
	}
	return requested[0]
}

func decodeCloudflarePublishRequest(w http.ResponseWriter, r *http.Request) (cloudflareAccessPolicy, error) {
	policy := cloudflareAccessPolicy{Mode: cloudflareAccessPublic}
	r.Body = http.MaxBytesReader(w, r.Body, maxCloudflarePublishBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var request cloudflarePublishRequest
	if err := decoder.Decode(&request); err != nil {
		if errors.Is(err, io.EOF) {
			return policy, nil
		}
		return cloudflareAccessPolicy{}, fmt.Errorf("invalid Cloudflare publish request: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return cloudflareAccessPolicy{}, errors.New("invalid Cloudflare publish request: expected one JSON object")
	}
	request.Visibility = strings.ToLower(strings.TrimSpace(request.Visibility))
	if request.Visibility == "" {
		request.Visibility = cloudflareAccessPublic
	}
	return normalizeCloudflareAccessPolicy(request.Visibility, request.Emails)
}

func normalizeCloudflareAccessPolicy(mode string, emails []string) (cloudflareAccessPolicy, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	switch mode {
	case cloudflareAccessPublic:
		if len(emails) != 0 {
			return cloudflareAccessPolicy{}, errors.New("email addresses are only valid for restricted Cloudflare publications")
		}
		return cloudflareAccessPolicy{Mode: mode}, nil
	case cloudflareAccessRestricted:
		if len(emails) == 0 {
			return cloudflareAccessPolicy{}, errors.New("restricted Cloudflare publications require at least one email address")
		}
		if len(emails) > maxCloudflareAccessEmails {
			return cloudflareAccessPolicy{}, fmt.Errorf("restricted Cloudflare publications allow at most %d email addresses", maxCloudflareAccessEmails)
		}
	default:
		return cloudflareAccessPolicy{}, errors.New("visibility must be public or restricted")
	}

	seen := make(map[string]struct{}, len(emails))
	normalized := make([]string, 0, len(emails))
	for _, raw := range emails {
		candidate := strings.ToLower(strings.TrimSpace(raw))
		if candidate == "" || len(candidate) > 254 {
			return cloudflareAccessPolicy{}, fmt.Errorf("invalid email address %q", raw)
		}
		address, err := mail.ParseAddress(candidate)
		if err != nil || !strings.EqualFold(address.Address, candidate) {
			return cloudflareAccessPolicy{}, fmt.Errorf("invalid email address %q", raw)
		}
		if _, exists := seen[candidate]; exists {
			continue
		}
		seen[candidate] = struct{}{}
		normalized = append(normalized, candidate)
	}
	sort.Strings(normalized)
	return cloudflareAccessPolicy{Mode: mode, Emails: normalized}, nil
}

type cloudflareAccessApplication struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Domain string `json:"domain"`
}

type cloudflareAccessApplicationSpec struct {
	Name         string
	Domain       string
	Destinations []string
	Emails       []string
	IdentityID   string
}

func (p *CloudflarePublisher) accessApplicationSpec(pub *cloudflarePublication, policy cloudflareAccessPolicy, includeCustomDomain bool) cloudflareAccessApplicationSpec {
	pagesDomain := pub.ProjectName + ".pages.dev"
	destinations := []string{pagesDomain, "*." + pagesDomain}
	domain := pagesDomain
	if includeCustomDomain {
		destinations = append([]string{pub.Hostname}, destinations...)
		domain = pub.Hostname
	}
	return cloudflareAccessApplicationSpec{
		Name:         "Spot: " + pub.Site,
		Domain:       domain,
		Destinations: destinations,
		Emails:       append([]string(nil), policy.Emails...),
		IdentityID:   p.cfg.AccessIDPID,
	}
}

func (p *CloudflarePublisher) ensureAccessApplication(ctx context.Context, pub *cloudflarePublication, policy cloudflareAccessPolicy, includeCustomDomain bool) error {
	if policy.Mode != cloudflareAccessRestricted {
		return nil
	}
	if !p.cfg.AccessEnabled() {
		return errors.New("Cloudflare Access email restriction is not configured")
	}
	spec := p.accessApplicationSpec(pub, policy, includeCustomDomain)
	if pub.AccessManaged {
		if pub.AccessAppID == "" {
			return errors.New("Cloudflare Access application ownership is missing its resource ID")
		}
		if err := p.client.UpdateAccessApplication(ctx, pub.AccountID, pub.AccessAppID, spec); err != nil {
			return err
		}
		pub.AccessMode = policy.Mode
		pub.AccessEmails = append([]string(nil), policy.Emails...)
		return p.storePublication(*pub)
	}
	if pub.AccessAppID != "" {
		return errors.New("Cloudflare Access application exists without cleanup ownership")
	}

	retryingCreate := pub.Status == "restricting"
	existing, err := p.findAccessApplication(ctx, pub.AccountID, spec)
	if err != nil {
		if retryingCreate {
			return errors.Join(errCloudflareAccessCreateUncertain, err)
		}
		return err
	}
	if existing != nil {
		if !retryingCreate {
			return fmt.Errorf("Cloudflare Access application %q already exists but is not managed by Spot", spec.Name)
		}
		// First persist cleanup ownership, then make the recovered application's
		// policy match this retry. The user may have changed the allowlist since
		// the interrupted request.
		pub.AccessAppID = existing.ID
		pub.AccessManaged = true
		if err := p.storePublication(*pub); err != nil {
			return errors.Join(errCloudflareAccessCreateUncertain, err)
		}
		if err := p.client.UpdateAccessApplication(ctx, pub.AccountID, existing.ID, spec); err != nil {
			return err
		}
		pub.AccessMode = policy.Mode
		pub.AccessEmails = append([]string(nil), policy.Emails...)
		pub.LastError = ""
		return p.storePublication(*pub)
	}
	if retryingCreate {
		// An empty list response is not proof that an earlier POST was rejected:
		// Access reads may be eventually consistent. Never issue a second create
		// from an uncertain state. A later publish can adopt the app once it is
		// visible, without risking a duplicate whose ID Spot cannot clean up.
		uncertainErr := errors.New("previous Cloudflare Access create is still unresolved; retry after the application becomes visible")
		pub.LastError = uncertainErr.Error()
		stateErr := p.storePublication(*pub)
		return errors.Join(errCloudflareAccessCreateUncertain, uncertainErr, stateErr)
	}

	// Persist an honest transition marker before the create. In particular, a
	// public publication remains recorded as public until Access returns an ID
	// and Spot has durable cleanup ownership.
	pub.Status = "restricting"
	pub.LastError = ""
	pub.AccessManaged = false
	if err := p.storePublication(*pub); err != nil {
		return err
	}
	app, createErr := p.client.CreateAccessApplication(ctx, pub.AccountID, spec)
	if createErr != nil && cloudflareRequestDefinitelyRejected(createErr) {
		// A 4xx/API rejection proves this POST did not create an application.
		// In particular, never adopt an app returned by a concurrent 409: Spot
		// has no ownership proof for that resource.
		return createErr
	}
	if createErr != nil || app.ID == "" {
		// A timeout, truncated response, or decode failure can still mean that
		// Cloudflare committed the create. Reconcile by the exact deterministic
		// application name/domain before allowing a retry to create another app.
		recovered, findErr := p.findAccessApplication(ctx, pub.AccountID, spec)
		if findErr != nil {
			return errors.Join(errCloudflareAccessCreateUncertain, createErr, findErr)
		}
		if recovered != nil {
			app = *recovered
		} else if createErr != nil {
			pub.LastError = createErr.Error()
			stateErr := p.storePublication(*pub)
			return errors.Join(errCloudflareAccessCreateUncertain, createErr, stateErr)
		} else {
			missingIDErr := errors.New("Cloudflare created an Access application without returning its resource ID")
			pub.LastError = missingIDErr.Error()
			stateErr := p.storePublication(*pub)
			return errors.Join(errCloudflareAccessCreateUncertain, missingIDErr, stateErr)
		}
	}
	return p.adoptAccessApplication(pub, policy, app)
}

func (p *CloudflarePublisher) findAccessApplication(ctx context.Context, accountID string, spec cloudflareAccessApplicationSpec) (*cloudflareAccessApplication, error) {
	apps, err := p.client.FindAccessApplications(ctx, accountID, spec.Name)
	if err != nil {
		return nil, err
	}
	var matched *cloudflareAccessApplication
	for _, app := range apps {
		if app.Name != spec.Name {
			continue
		}
		if app.Domain != spec.Domain {
			return nil, fmt.Errorf("Cloudflare Access application %q exists for unexpected domain %q", spec.Name, app.Domain)
		}
		if matched != nil {
			return nil, fmt.Errorf("multiple Cloudflare Access applications named %q exist", spec.Name)
		}
		candidate := app
		matched = &candidate
	}
	return matched, nil
}

func (p *CloudflarePublisher) reconcileUncertainAccessApplication(ctx context.Context, pub *cloudflarePublication) error {
	// A first restricted publication creates Access for pages.dev before the
	// custom domain exists. Transitions of an existing publication create the
	// application with the custom hostname as its primary domain.
	includeCustomDomain := pub.DeploymentID != ""
	spec := p.accessApplicationSpec(pub, cloudflareAccessPolicy{Mode: cloudflareAccessRestricted}, includeCustomDomain)
	app, err := p.findAccessApplication(ctx, pub.AccountID, spec)
	if err != nil {
		return errors.Join(errCloudflareAccessCreateUncertain, err)
	}
	if app == nil {
		return errors.Join(errCloudflareAccessCreateUncertain,
			errors.New("previous Cloudflare Access create is still unresolved; retry after the application becomes visible"))
	}
	pub.AccessAppID = app.ID
	pub.AccessManaged = true
	if err := p.storePublication(*pub); err != nil {
		return errors.Join(errCloudflareAccessCreateUncertain, err)
	}
	return nil
}

// resolveUncertainAccessApplication provides an explicit recovery path for an
// Access create whose response was lost and whose exact application is still
// absent from Cloudflare's list API. Spot can safely adopt a matching app on its
// own. Clearing an empty result requires the site owner to attest that they also
// checked Cloudflare Zero Trust, because an empty API response alone is not proof
// that an earlier POST never committed.
func (p *CloudflarePublisher) resolveUncertainAccessApplication(ctx context.Context, site string, confirmAbsent bool) (*cloudflarePublication, string, error) {
	if p == nil || p.repo == nil || !p.cfg.Enabled() || p.client == nil {
		return nil, "", errCloudflareNotConfigured
	}
	pub, err := p.repo.Get(ctx, site)
	if err != nil {
		return nil, "", err
	}
	if pub == nil {
		return nil, "", ErrSiteNotFound
	}
	if pub.CleanupUnknown {
		return nil, "", errCloudflareCleanupUnknown
	}
	if pub.Status != "restricting" || pub.AccessManaged || pub.AccessAppID != "" {
		return nil, "", errCloudflareAccessNotUncertain
	}

	accountID, _ := p.publicationLocation(pub)
	pub.AccountID = accountID
	includeCustomDomain := pub.DeploymentID != ""
	spec := p.accessApplicationSpec(pub, cloudflareAccessPolicy{Mode: cloudflareAccessRestricted}, includeCustomDomain)
	app, err := p.findAccessApplication(ctx, accountID, spec)
	if err != nil {
		return nil, "", errors.Join(errCloudflareAccessCreateUncertain, err)
	}
	if app != nil {
		pub.AccessAppID = app.ID
		pub.AccessManaged = true
		pub.LastError = "Cloudflare Access application recovered; resume publish or unpublish"
		if err := p.storePublication(*pub); err != nil {
			return nil, "", errors.Join(errCloudflareAccessCreateUncertain, err)
		}
		return pub, "adopted", nil
	}
	if !confirmAbsent {
		return nil, "", errors.New("confirm that no matching Cloudflare Access application exists")
	}

	pub.Status = "failed"
	pub.LastError = "Uncertain Cloudflare Access create was manually confirmed absent; retry publish or unpublish"
	if err := p.storePublication(*pub); err != nil {
		return nil, "", err
	}
	return pub, "cleared", nil
}

func (p *CloudflarePublisher) adoptAccessApplication(pub *cloudflarePublication, policy cloudflareAccessPolicy, app cloudflareAccessApplication) error {
	if app.ID == "" {
		return errors.New("Cloudflare Access application is missing its resource ID")
	}
	pub.AccessAppID = app.ID
	pub.AccessManaged = true
	pub.AccessMode = policy.Mode
	pub.AccessEmails = append([]string(nil), policy.Emails...)
	pub.LastError = ""
	if err := p.storePublication(*pub); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cleanupErr := p.client.DeleteAccessApplication(cleanupCtx, pub.AccountID, app.ID)
		if errors.Is(cleanupErr, errCloudflareNotFound) {
			cleanupErr = nil
		}
		if cleanupErr != nil {
			return errors.Join(errCloudflareAccessCreateUncertain, err, cleanupErr)
		}
		// Cleanup definitely removed the app, so the durable create marker is no
		// longer truthful. Reset it to a normal retryable failure; otherwise an
		// eventually consistent empty lookup would block every future operation.
		stateErr := err
		pub.AccessAppID = ""
		pub.AccessManaged = false
		pub.AccessMode = cloudflareAccessPublic
		pub.AccessEmails = nil
		pub.Status = "failed"
		pub.LastError = "Cloudflare Access app was removed after its state could not be saved: " + stateErr.Error()
		return errors.Join(stateErr, p.storePublication(*pub))
	}
	return nil
}

func (p *CloudflarePublisher) deleteAccessApplication(ctx context.Context, pub *cloudflarePublication) error {
	if !pub.AccessManaged {
		if pub.AccessAppID != "" {
			return errors.New("refusing to delete an unmanaged Cloudflare Access application")
		}
		pub.AccessMode = cloudflareAccessPublic
		pub.AccessEmails = nil
		return nil
	}
	if pub.AccessAppID == "" {
		return errors.New("Cloudflare Access application ownership is missing its resource ID")
	}
	if err := p.client.DeleteAccessApplication(ctx, pub.AccountID, pub.AccessAppID); err != nil && !errors.Is(err, errCloudflareNotFound) {
		return err
	}
	pub.AccessManaged = false
	pub.AccessAppID = ""
	pub.AccessMode = cloudflareAccessPublic
	pub.AccessEmails = nil
	return nil
}

func (p *CloudflarePublisher) waitForDomainActive(ctx context.Context, accountID, projectName, hostname string) error {
	timeout := p.domainActivationTimeout
	if timeout <= 0 {
		timeout = cloudflareDomainActiveTimeout
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		domain, err := p.client.GetDomain(waitCtx, accountID, projectName, hostname)
		if err != nil {
			return err
		}
		switch strings.ToLower(domain.Status) {
		case "active":
			return nil
		case "deactivated", "blocked", "error":
			return fmt.Errorf("Cloudflare Pages domain %s entered terminal status %q", hostname, domain.Status)
		case "initializing", "pending":
		default:
			return fmt.Errorf("Cloudflare Pages domain %s returned unknown status %q", hostname, domain.Status)
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("wait for Cloudflare Pages domain %s: %w", hostname, waitCtx.Err())
		case <-ticker.C:
		}
	}
}

// waitForAccessProtection verifies edge behavior, not just the control-plane
// response. A first restricted publication keeps non-sensitive placeholder
// content deployed until every public hostname shape challenges an anonymous
// browser request.
func (p *CloudflarePublisher) waitForAccessProtection(ctx context.Context, pub *cloudflarePublication) error {
	timeout := p.accessActivationTimeout
	if timeout <= 0 {
		timeout = cloudflareAccessActiveTimeout
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	urls := []string{
		"https://" + pub.Hostname + "/",
		"https://" + pub.ProjectName + ".pages.dev/",
		"https://spot-access-check." + pub.ProjectName + ".pages.dev/",
	}
	var lastErr error
	for {
		ready := true
		for _, rawURL := range urls {
			challenged, err := p.client.AccessChallenge(waitCtx, rawURL)
			if err != nil {
				lastErr = fmt.Errorf("probe %s: %w", rawURL, err)
				ready = false
				break
			}
			if !challenged {
				lastErr = fmt.Errorf("%s did not return a Cloudflare Access challenge", rawURL)
				ready = false
				break
			}
		}
		if ready {
			return nil
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			return fmt.Errorf("wait for Cloudflare Access edge protection: %w", errors.Join(waitCtx.Err(), lastErr))
		case <-timer.C:
		}
	}
}

// AccessChallenge makes an unauthenticated browser-shaped request and refuses
// redirects so callers can distinguish an Access login from origin content.
func (c *CloudflareClient) AccessChallenge(ctx context.Context, rawURL string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Spot-Access-Check/1.0)")
	client := *c.httpClient()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4<<10))
	if res.StatusCode < 300 || res.StatusCode > 399 {
		return false, nil
	}
	location, err := url.Parse(res.Header.Get("Location"))
	if err != nil {
		return false, nil
	}
	host := strings.ToLower(location.Hostname())
	return strings.HasSuffix(host, ".cloudflareaccess.com") ||
		strings.Contains(location.Path, "/cdn-cgi/access/"), nil
}

func accessApplicationPayload(spec cloudflareAccessApplicationSpec) map[string]any {
	destinations := make([]map[string]string, 0, len(spec.Destinations))
	for _, destination := range spec.Destinations {
		destinations = append(destinations, map[string]string{"type": "public", "uri": destination})
	}
	include := make([]map[string]map[string]string, 0, len(spec.Emails))
	for _, email := range spec.Emails {
		include = append(include, map[string]map[string]string{"email": {"email": email}})
	}
	return map[string]any{
		"name":                      spec.Name,
		"domain":                    spec.Domain,
		"type":                      "self_hosted",
		"destinations":              destinations,
		"session_duration":          "24h",
		"app_launcher_visible":      false,
		"allowed_idps":              []string{spec.IdentityID},
		"auto_redirect_to_identity": true,
		"policies": []map[string]any{{
			"name":       "Spot email allowlist",
			"decision":   "allow",
			"precedence": 1,
			"include":    include,
		}},
	}
}

func (c *CloudflareClient) CreateAccessApplication(ctx context.Context, accountID string, spec cloudflareAccessApplicationSpec) (cloudflareAccessApplication, error) {
	body, err := json.Marshal(accessApplicationPayload(spec))
	if err != nil {
		return cloudflareAccessApplication{}, err
	}
	var app cloudflareAccessApplication
	err = c.do(ctx, http.MethodPost, "/accounts/"+url.PathEscape(accountID)+"/access/apps", "", body, "application/json", &app)
	return app, err
}

func (c *CloudflareClient) FindAccessApplications(ctx context.Context, accountID, name string) ([]cloudflareAccessApplication, error) {
	query := url.Values{}
	query.Set("name", name)
	query.Set("exact", "true")
	query.Set("per_page", "50")
	var apps []cloudflareAccessApplication
	err := c.do(ctx, http.MethodGet, "/accounts/"+url.PathEscape(accountID)+"/access/apps?"+query.Encode(), "", nil, "", &apps)
	return apps, err
}

func (c *CloudflareClient) UpdateAccessApplication(ctx context.Context, accountID, appID string, spec cloudflareAccessApplicationSpec) error {
	body, err := json.Marshal(accessApplicationPayload(spec))
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPut, "/accounts/"+url.PathEscape(accountID)+"/access/apps/"+url.PathEscape(appID), "", body, "application/json", nil)
}

func (c *CloudflareClient) DeleteAccessApplication(ctx context.Context, accountID, appID string) error {
	return c.do(ctx, http.MethodDelete, "/accounts/"+url.PathEscape(accountID)+"/access/apps/"+url.PathEscape(appID), "", nil, "", nil)
}
