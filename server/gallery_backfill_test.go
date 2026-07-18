package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
)

type cancelAfterSitePut struct {
	SiteStorage
	cancel context.CancelFunc
}

func (s *cancelAfterSitePut) Put(ctx context.Context, site, path, contentType string, data []byte) error {
	if err := s.SiteStorage.Put(ctx, site, path, contentType, data); err != nil {
		return err
	}
	s.cancel()
	return nil
}

func TestBackfillGalleryWritesMetadataAndSpotJSONWithoutTouchingOwner(t *testing.T) {
	ctx := context.Background()
	db, err := openSQLiteDB(ctx, filepath.Join(t.TempDir(), "spot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	registry := NewSiteRegistry(db, nil)
	owner := Identity{Email: "alice@example.com", Name: "Alice"}
	authz, err := registry.AuthorizeDeploy(ctx, "demo", owner)
	if err != nil {
		t.Fatalf("claim site: %v", err)
	}
	before, err := registry.AllSites(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 {
		t.Fatalf("sites before = %d, want 1", len(before))
	}

	dir := t.TempDir()
	writeSiteFile(t, dir, "demo", "index.html", `<html><head><title>Demo App</title><meta name="description" content="A useful demo"></head><body><h1>Demo</h1></body></html>`)
	sites, err := NewLocalSiteStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.RecordDeploy(ctx, DeployAuditEvent{
		Site: "demo", Actor: owner, Action: "create", Status: "success", ContentHash: "pre-backfill-hash",
		ContentGeneration: authz.ContentGeneration,
	}); err != nil {
		t.Fatalf("record initial deploy: %v", err)
	}
	ownedBefore, err := registry.SitesOwnedBy(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(ownedBefore) != 1 || ownedBefore[0].ContentHashUncertain {
		t.Fatalf("hash before backfill = %+v, want current", ownedBefore)
	}
	srv := &Server{sites: sites, spotDomain: "spot.localhost"}

	result, err := srv.backfillGallery(ctx, registry, galleryBackfillOptions{Write: true, WriteSpotJSON: true})
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if result.MetadataUpdated != 1 || result.SpotJSONWritten != 1 {
		t.Fatalf("result = %+v, want one metadata update and one _spot.json write", result)
	}

	after, err := registry.AllSites(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 {
		t.Fatalf("sites after = %d, want 1", len(after))
	}
	site := after[0]
	if site.OwnerEmail != "alice@example.com" || site.OwnerName != "Alice" {
		t.Fatalf("owner after backfill = %q/%q, want Alice", site.OwnerEmail, site.OwnerName)
	}
	if !site.UpdatedAt.Equal(before[0].UpdatedAt) {
		t.Fatalf("updated_at changed from %s to %s", before[0].UpdatedAt, site.UpdatedAt)
	}
	if site.Title != "Demo App" || site.Description != "A useful demo" {
		t.Fatalf("metadata = %q/%q, want extracted title and description", site.Title, site.Description)
	}

	raw, err := readSiteFile(ctx, sites, "demo", siteMetadataFileName)
	if err != nil {
		t.Fatalf("read _spot.json: %v", err)
	}
	var meta SiteMetadata
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("parse written _spot.json: %v", err)
	}
	if meta.Title != "Demo App" || meta.Description != "A useful demo" {
		t.Fatalf("_spot.json metadata = %+v, want extracted metadata", meta)
	}
	ownedAfter, err := registry.SitesOwnedBy(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(ownedAfter) != 1 || !ownedAfter[0].ContentHashUncertain || ownedAfter[0].ContentHash != "pre-backfill-hash" {
		t.Fatalf("hash after served-file backfill = %+v, want prior hash marked uncertain", ownedAfter)
	}
}

func TestExternalBackfillMutationCannotBeClearedByConcurrentDeploy(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	registry := NewSiteRegistry(db, nil)
	owner := Identity{Email: "alice@example.com", Name: "Alice"}
	initial, err := registry.AuthorizeDeploy(ctx, "demo", owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.RecordDeploy(ctx, DeployAuditEvent{
		Site: "demo", Actor: owner, Action: "create", Status: "success",
		ContentHash: "initial", ContentGeneration: initial.ContentGeneration,
	}); err != nil {
		t.Fatal(err)
	}

	firstLease, err := registry.BeginExternalContentMutation(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	concurrent, err := registry.AuthorizeDeploy(ctx, "demo", owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.RecordDeploy(ctx, DeployAuditEvent{
		Site: "demo", Actor: owner, Action: "update", Status: "success",
		ContentHash: "concurrent", ContentGeneration: concurrent.ContentGeneration,
	}); err != nil {
		t.Fatal(err)
	}
	if err := registry.EndExternalContentMutation(ctx, "demo", firstLease); err != nil {
		t.Fatal(err)
	}
	owned, err := registry.SitesOwnedBy(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(owned) != 1 || !owned[0].ContentHashUncertain {
		t.Fatalf("site after interleaved backfill/deploy = %+v, want uncertain hash", owned)
	}

	// The inverse completion order is also unsafe without generations: the
	// backfill closes after writing, then the older deploy audit arrives late.
	secondLease, err := registry.BeginExternalContentMutation(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	late, err := registry.AuthorizeDeploy(ctx, "demo", owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.EndExternalContentMutation(ctx, "demo", secondLease); err != nil {
		t.Fatal(err)
	}
	if err := registry.RecordDeploy(ctx, DeployAuditEvent{
		Site: "demo", Actor: owner, Action: "update", Status: "success",
		ContentHash: "late-audit", ContentGeneration: late.ContentGeneration,
	}); err != nil {
		t.Fatal(err)
	}
	owned, err = registry.SitesOwnedBy(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(owned) != 1 || !owned[0].ContentHashUncertain {
		t.Fatalf("site after late concurrent deploy audit = %+v, want uncertain hash", owned)
	}

	// A later deploy that starts after the external mutation closes owns the
	// current generation and can safely establish a new current hash.
	recovery, err := registry.AuthorizeDeploy(ctx, "demo", owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.RecordDeploy(ctx, DeployAuditEvent{
		Site: "demo", Actor: owner, Action: "update", Status: "success",
		ContentHash: "recovered", ContentGeneration: recovery.ContentGeneration,
	}); err != nil {
		t.Fatal(err)
	}
	owned, err = registry.SitesOwnedBy(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(owned) != 1 || owned[0].ContentHashUncertain || owned[0].ContentHash != "recovered" {
		t.Fatalf("site after recovery deploy = %+v, want current recovered hash", owned)
	}
}

func TestBackfillClosesExternalMutationAfterCommandCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	db := openTestDB(t)
	registry := NewSiteRegistry(db, nil)
	owner := Identity{Email: "alice@example.com", Name: "Alice"}
	if _, err := registry.AuthorizeDeploy(ctx, "demo", owner); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeSiteFile(t, dir, "demo", "index.html", "<title>Demo</title>")
	local, err := NewLocalSiteStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		sites:      &cancelAfterSitePut{SiteStorage: local, cancel: cancel},
		spotDomain: "spot.localhost",
	}
	if _, err := srv.backfillGallery(ctx, registry, galleryBackfillOptions{Write: true, WriteSpotJSON: true}); err != nil {
		t.Fatalf("backfill after cancellation during put: %v", err)
	}
	var active bool
	var dirty bool
	if err := db.QueryRowContext(context.Background(), `SELECT content_external_mutation, content_dirty
		FROM sites WHERE name = 'demo'`).Scan(&active, &dirty); err != nil {
		t.Fatal(err)
	}
	if active || !dirty {
		t.Fatalf("external mutation active=%v dirty=%v, want closed and dirty", active, dirty)
	}
}

func TestExternalContentMutationLeaseRejectsOverlapAndWrongOwner(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	registry := NewSiteRegistry(db, nil)
	if _, err := registry.AuthorizeDeploy(ctx, "demo", Identity{Email: "alice@example.com"}); err != nil {
		t.Fatal(err)
	}
	owner, err := registry.BeginExternalContentMutation(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.BeginExternalContentMutation(ctx, "demo"); !errors.Is(err, ErrExternalContentMutationActive) {
		t.Fatalf("overlapping begin = %v, want ErrExternalContentMutationActive", err)
	}
	if err := registry.EndExternalContentMutation(ctx, "demo", "not-the-owner"); !errors.Is(err, ErrExternalContentMutationLeaseLost) {
		t.Fatalf("wrong-owner end = %v, want ErrExternalContentMutationLeaseLost", err)
	}
	var active bool
	if err := db.QueryRowContext(ctx, `SELECT content_external_mutation FROM sites WHERE name = 'demo'`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if !active {
		t.Fatal("wrong owner cleared the active mutation lease")
	}
	if err := registry.EndExternalContentMutation(ctx, "demo", owner); err != nil {
		t.Fatal(err)
	}
}

func TestBackfillRecoversStaleExternalMutationWhenNoFilesNeedWriting(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	registry := NewSiteRegistry(db, nil)
	owner := Identity{Email: "alice@example.com", Name: "Alice"}
	if _, err := registry.AuthorizeDeploy(ctx, "demo", owner); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeSiteFile(t, dir, "demo", "index.html", "<title>Demo</title>")
	writeSiteFile(t, dir, "demo", siteMetadataFileName, `{"title":"Demo"}`)
	local, err := NewLocalSiteStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.BeginExternalContentMutation(ctx, "demo"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE sites
		SET content_external_mutation_started_at = unixepoch() - 7200 WHERE name = 'demo'`); err != nil {
		t.Fatal(err)
	}
	srv := &Server{sites: local, spotDomain: "spot.localhost"}
	result, err := srv.backfillGallery(ctx, registry, galleryBackfillOptions{Write: true, WriteSpotJSON: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.SpotJSONWritten != 0 {
		t.Fatalf("result = %+v, want no served-file write", result)
	}
	var active bool
	var startedAt int64
	if err := db.QueryRowContext(ctx, `SELECT content_external_mutation, content_external_mutation_started_at
		FROM sites WHERE name = 'demo'`).Scan(&active, &startedAt); err != nil {
		t.Fatal(err)
	}
	if active || startedAt != 0 {
		t.Fatalf("stale lease active=%v started_at=%d, want recovered", active, startedAt)
	}
}

func TestBackfillScreenshotCaptureFailureDoesNotDirtyCurrentHash(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	registry := NewSiteRegistry(db, nil)
	owner := Identity{Email: "alice@example.com", Name: "Alice"}
	authz, err := registry.AuthorizeDeploy(ctx, "demo", owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.RecordDeploy(ctx, DeployAuditEvent{
		Site: "demo", Actor: owner, Action: "create", Status: "success",
		ContentHash: "current", ContentGeneration: authz.ContentGeneration,
	}); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	writeSiteFile(t, dir, "demo", "index.html", "<title>Demo</title>")
	writeSiteFile(t, dir, "demo", siteMetadataFileName, `{"title":"Demo"}`)
	local, err := NewLocalSiteStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{sites: local, spotDomain: "spot.localhost"}
	_, err = srv.backfillGallery(ctx, registry, galleryBackfillOptions{
		Write:       true,
		Screenshots: true,
		Scheme:      "http",
		captureScreenshot: func(context.Context, string, string) ([]byte, error) {
			return nil, errors.New("capture failed")
		},
	})
	if err == nil {
		t.Fatal("backfill succeeded, want screenshot capture failure")
	}

	owned, err := registry.SitesOwnedBy(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(owned) != 1 || owned[0].ContentHashUncertain || owned[0].ContentHash != "current" {
		t.Fatalf("site after screenshot capture failure = %+v, want current hash preserved", owned)
	}
	var active bool
	if err := db.QueryRowContext(ctx, `SELECT content_external_mutation FROM sites WHERE name = 'demo'`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active {
		t.Fatal("screenshot capture failure left an external mutation active")
	}
}

func TestBackfillRevalidatesMetadataAfterScreenshotCapture(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	registry := NewSiteRegistry(db, nil)
	owner := Identity{Email: "alice@example.com", Name: "Alice"}
	if _, err := registry.AuthorizeDeploy(ctx, "demo", owner); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeSiteFile(t, dir, "demo", "index.html", "<title>Old Deploy</title>")
	local, err := NewLocalSiteStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{sites: local, spotDomain: "spot.localhost"}
	newSpotJSON := []byte(`{"title":"New Deploy","description":"deployed during capture"}`)
	png := []byte("\x89PNG\r\n\x1a\nnew-preview")

	result, err := srv.backfillGallery(ctx, registry, galleryBackfillOptions{
		Write:         true,
		WriteSpotJSON: true,
		Screenshots:   true,
		Scheme:        "http",
		captureScreenshot: func(context.Context, string, string) ([]byte, error) {
			if err := local.Put(ctx, "demo", "index.html", "text/html", []byte("<title>New Deploy</title>")); err != nil {
				return nil, err
			}
			if err := local.Put(ctx, "demo", siteMetadataFileName, "application/json", newSpotJSON); err != nil {
				return nil, err
			}
			if err := registry.UpdateSiteMetadata(ctx, "demo", SiteMetadata{
				Title: "New Deploy", Description: "deployed during capture",
			}); err != nil {
				return nil, err
			}
			return png, nil
		},
	})
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if result.SpotJSONWritten != 0 || result.ScreenshotsWritten != 1 {
		t.Fatalf("result = %+v, want new deploy metadata preserved and screenshot written", result)
	}
	raw, err := readSiteFile(ctx, local, "demo", siteMetadataFileName)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, newSpotJSON) {
		t.Fatalf("_spot.json = %s, want concurrent deploy bytes %s", raw, newSpotJSON)
	}
	meta, err := registry.SiteMetadata(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Title != "New Deploy" || meta.Description != "deployed during capture" {
		t.Fatalf("registry metadata = %+v, want concurrent deploy metadata", meta)
	}
}

func TestBackfillGalleryScreenshotsOnlyPublicSites(t *testing.T) {
	ctx := context.Background()
	db, err := openSQLiteDB(ctx, filepath.Join(t.TempDir(), "spot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	registry := NewSiteRegistry(db, nil)
	owner := Identity{Email: "alice@example.com", Name: "Alice"}
	for _, site := range []string{"open", "locked"} {
		if _, err := registry.AuthorizeDeploy(ctx, site, owner); err != nil {
			t.Fatalf("claim %s: %v", site, err)
		}
	}
	dir := t.TempDir()
	writeSiteFile(t, dir, "open", "index.html", "<title>Open</title>")
	writeSiteFile(t, dir, "locked", "index.html", "<title>Locked</title>")
	writeSiteFile(t, dir, "locked", accessFileName, `{"allow":["alice@example.com"]}`)
	sites, err := NewLocalSiteStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{sites: sites, spotDomain: "spot.localhost"}
	png := []byte("\x89PNG\r\n\x1a\nfake-png-body")

	result, err := srv.backfillGallery(ctx, registry, galleryBackfillOptions{
		Write:       true,
		Screenshots: true,
		Scheme:      "http",
		captureScreenshot: func(_ context.Context, site, url string) ([]byte, error) {
			if site != "open" {
				t.Fatalf("captured restricted site %s", site)
			}
			if url != "http://open.spot.localhost/" {
				t.Fatalf("screenshot url = %q, want open site URL", url)
			}
			return png, nil
		},
	})
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if result.ScreenshotsWritten != 1 || result.ScreenshotsSkipped != 1 {
		t.Fatalf("result = %+v, want one screenshot and one restricted skip", result)
	}
	got, err := readSiteFile(ctx, sites, "open", "_screenshot.png")
	if err != nil {
		t.Fatalf("read screenshot: %v", err)
	}
	if !bytes.Equal(got, png) {
		t.Fatalf("screenshot bytes = %q, want generated png", got)
	}
	if exists, err := siteFileExists(ctx, sites, "locked", "_screenshot.png"); err != nil || exists {
		t.Fatalf("restricted screenshot exists=%v err=%v, want no file", exists, err)
	}
}
