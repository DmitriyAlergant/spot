package main

import (
	"os"
	"strings"
	"testing"
)

func TestSpotsUIUsesCloudflarePublicationStatus(t *testing.T) {
	for _, path := range []string{"../sdk/spots.html", "static_assets/sdk/spots.html"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		page := string(raw)
		for _, want := range []string{
			"cf.publication.status === 'published'",
			"cf.publication.status === 'failed'",
			"'publishing'",
			"'claiming-project'",
			"'restricting'",
			"'protecting-custom-domain'",
			"'activating-domain'",
			"'deploying-restricted'",
			"'deleting-project'",
			"'project-deleted'",
			"hasUncertainProjectState",
			"Retry publish",
			"Internet publishing failed:",
			"cf.publication.project_managed === false && cf.publication.dns_managed === false && cf.publication.access_managed === false",
			"cf.publication.project_managed || cf.publication.dns_managed || cf.publication.access_managed",
			"cf.publication.cleanup_unknown",
			"The legacy internet cleanup location is unknown",
			"cf-publish-dialog",
			"PIN by email",
			"This is separate from who can open it on the mesh",
			"visibility, emails",
			"cf.publication.access_mode === 'restricted'",
			"publication.requested_access_mode || publication.access_mode",
			"publication.requested_access_emails || publication.access_emails",
			"Resolve Access state",
			"confirm_absent: true",
			"Resolve project state",
			"confirm_resources_removed: true",
			"Resolve legacy state",
			"Internet version needs update:",
			"const hashKnown = Boolean(cf.content_hash)",
			"internetPublishingError",
			"cf.operation_active",
			"Publishing internet version… It is safe to leave or reload this page.",
			"scheduleCloudflarePoll",
			"loadMine(true)",
			"Publishing is still in progress",
		} {
			if !strings.Contains(page, want) {
				t.Fatalf("%s does not contain %q", path, want)
			}
		}
		for _, old := range []string{
			"Cloudflare live:",
			"Cloudflare needs update:",
			"Publish to Cloudflare",
			"Published to Cloudflare",
			"Cloudflare settings for ",
		} {
			if strings.Contains(page, old) {
				t.Fatalf("%s still contains user-facing provider copy %q", path, old)
			}
		}
	}
}
