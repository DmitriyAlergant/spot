package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type galleryBackfillOptions struct {
	Write         bool
	WriteSpotJSON bool
	Force         bool
	AITags        bool
	Screenshots   bool
	Scheme        string
	Chrome        string
	Site          string

	captureScreenshot func(context.Context, string, string) ([]byte, error)
}

type galleryBackfillResult struct {
	Sites              int
	MetadataUpdated    int
	SpotJSONWritten    int
	ScreenshotsWritten int
	ScreenshotsSkipped int
}

type galleryBackfillSiteResult struct {
	MetadataUpdated   bool
	SpotJSONWritten   bool
	ScreenshotWritten bool
	ScreenshotSkipped bool
}

type galleryBackfillSitePlan struct {
	result     galleryBackfillSiteResult
	merged     SiteMetadata
	restricted bool
}

func runGalleryBackfillCommand(ctx context.Context, args []string) error {
	cfg := defaultConfigFromEnv()
	opts := galleryBackfillOptions{
		WriteSpotJSON: true,
		AITags:        true,
		Scheme:        "https",
	}
	fs := flag.NewFlagSet("backfill-gallery", flag.ContinueOnError)
	fs.BoolVar(&opts.Write, "write", opts.Write, "write changes; without this, only log planned changes")
	fs.BoolVar(&opts.WriteSpotJSON, "write-spot-json", opts.WriteSpotJSON, "write missing _spot.json files into site storage")
	fs.BoolVar(&opts.Force, "force", opts.Force, "replace existing gallery metadata, _spot.json, and screenshots")
	fs.BoolVar(&opts.AITags, "ai-tags", opts.AITags, "use configured AI to suggest tags for public sites without tags")
	fs.BoolVar(&opts.Screenshots, "screenshots", opts.Screenshots, "capture missing _screenshot.png files for public sites")
	fs.StringVar(&opts.Scheme, "scheme", opts.Scheme, "public site URL scheme used for screenshots")
	fs.StringVar(&opts.Chrome, "chrome", opts.Chrome, "path to chromium/google-chrome for screenshots")
	fs.StringVar(&opts.Site, "site", opts.Site, "only backfill one site")
	fs.StringVar(&cfg.StorageMode, "storage", cfg.StorageMode, "storage mode: s3 or local")
	fs.StringVar(&cfg.SpotDomain, "domain", cfg.SpotDomain, "apex domain for Spot")
	fs.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir, "data directory for SQLite and local storage")
	fs.StringVar(&cfg.SQLitePath, "sqlite", cfg.SQLitePath, "SQLite database path")
	fs.StringVar(&cfg.SitesDir, "sites-dir", cfg.SitesDir, "local site storage directory")
	fs.StringVar(&cfg.S3Endpoint, "s3-endpoint", cfg.S3Endpoint, "S3-compatible endpoint")
	fs.StringVar(&cfg.S3AccessKey, "s3-access-key", cfg.S3AccessKey, "S3 access key")
	fs.StringVar(&cfg.S3SecretKey, "s3-secret-key", cfg.S3SecretKey, "S3 secret key")
	fs.StringVar(&cfg.SitesBucket, "sites-bucket", cfg.SitesBucket, "S3 bucket containing deployed sites")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg.StorageMode = normalizeStorageMode(cfg.StorageMode)
	if err := finalizeConfig(&cfg); err != nil {
		return err
	}
	if opts.Site != "" && !siteNameRe.MatchString(opts.Site) {
		return fmt.Errorf("invalid site %q", opts.Site)
	}

	db, err := openSQLiteDB(ctx, cfg.SQLitePath)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer db.Close()
	registry := NewSiteRegistry(db, nil)

	sites, err := openBackfillSiteStore(cfg)
	if err != nil {
		return err
	}
	var ai *AIProxy
	if opts.AITags && cfg.OpenAIAPIKey != "" {
		ai = NewAIProxy(cfg.OpenAIAPIKey, cfg.OpenAIBaseURL, cfg.AIModel, cfg.AIAllowedModels, cfg.AIAllowedImageModels)
	}
	srv := &Server{
		sites:      sites,
		deployAuth: registry,
		ai:         ai,
		spotDomain: cfg.SpotDomain,
	}

	if !opts.Write {
		log.Printf("backfill-gallery: dry run; pass -write to update SQLite or site storage")
	}
	result, err := srv.backfillGallery(ctx, registry, opts)
	if err != nil {
		return err
	}
	log.Printf("backfill-gallery: sites=%d metadata=%d spot_json=%d screenshots=%d screenshots_skipped=%d",
		result.Sites, result.MetadataUpdated, result.SpotJSONWritten, result.ScreenshotsWritten, result.ScreenshotsSkipped)
	return nil
}

func openBackfillSiteStore(cfg config) (SiteStorage, error) {
	if cfg.StorageMode == storageModeLocal {
		sites, err := NewLocalSiteStore(cfg.SitesDir)
		if err != nil {
			return nil, fmt.Errorf("site store: %w", err)
		}
		return sites, nil
	}
	sites, err := NewSiteStore(cfg.S3Endpoint, cfg.S3AccessKey, cfg.S3SecretKey, cfg.SitesBucket)
	if err != nil {
		return nil, fmt.Errorf("site store: %w", err)
	}
	return sites, nil
}

func (s *Server) backfillGallery(ctx context.Context, registry *SiteRegistry, opts galleryBackfillOptions) (galleryBackfillResult, error) {
	all, err := registry.AllSites(ctx)
	if err != nil {
		return galleryBackfillResult{}, err
	}
	var result galleryBackfillResult
	for _, site := range all {
		if opts.Site != "" && site.Name != opts.Site {
			continue
		}
		result.Sites++
		siteResult, err := s.backfillGallerySite(ctx, registry, site, opts)
		if err != nil {
			return result, err
		}
		if siteResult.MetadataUpdated {
			result.MetadataUpdated++
		}
		if siteResult.SpotJSONWritten {
			result.SpotJSONWritten++
		}
		if siteResult.ScreenshotWritten {
			result.ScreenshotsWritten++
		}
		if siteResult.ScreenshotSkipped {
			result.ScreenshotsSkipped++
		}
	}
	return result, nil
}

func (s *Server) backfillGallerySite(ctx context.Context, registry *SiteRegistry, site SiteRecord, opts galleryBackfillOptions) (galleryBackfillSiteResult, error) {
	if opts.Write {
		// A killed backfill cannot run its deferred close. A later invocation
		// may legitimately find nothing left to write, so expire an hour-old
		// lease before planning work while leaving recent/live runs protected.
		if err := registry.RecoverStaleExternalContentMutation(ctx, site.Name, time.Now().Add(-time.Hour)); err != nil {
			return galleryBackfillSiteResult{}, err
		}
	}
	plan, err := s.planGalleryBackfillSite(ctx, site, opts)
	if err != nil {
		return galleryBackfillSiteResult{}, err
	}

	if !opts.Write {
		log.Printf("backfill-gallery: %s metadata=%v spot_json=%v screenshot=%v restricted=%v",
			site.Name, plan.result.MetadataUpdated, plan.result.SpotJSONWritten, plan.result.ScreenshotWritten, plan.restricted)
		return plan.result, nil
	}
	var screenshotData []byte
	if plan.result.ScreenshotWritten {
		data, err := captureBackfillScreenshot(ctx, opts, site.Name, backfillSiteURL(opts.Scheme, s.spotDomain, site.Name))
		if err != nil {
			return galleryBackfillSiteResult{}, fmt.Errorf("%s: capture screenshot: %w", site.Name, err)
		}
		screenshotData = data

		// Screenshot capture is intentionally outside the mutation lease and can
		// take several seconds. Re-read both registry and deployed metadata after
		// it completes so a deploy that finished during capture wins over this
		// backfill instead of having its newer _spot.json overwritten.
		current, err := registry.SiteMetadata(ctx, site.Name)
		if err != nil {
			return galleryBackfillSiteResult{}, err
		}
		site.Title = current.Title
		site.Description = current.Description
		site.Tags = cloneSiteTags(current.Tags)
		plan, err = s.planGalleryBackfillSite(ctx, site, opts)
		if err != nil {
			return galleryBackfillSiteResult{}, err
		}
		if !plan.result.ScreenshotWritten {
			screenshotData = nil
		}
	}
	if plan.result.MetadataUpdated {
		if err := registry.UpdateSiteMetadata(ctx, site.Name, plan.merged); err != nil {
			return galleryBackfillSiteResult{}, err
		}
	}
	if plan.result.SpotJSONWritten || plan.result.ScreenshotWritten {
		// The backfill command runs out of process from the API. Bracket its
		// served-file writes durably so a concurrent deploy cannot clear an old
		// hash after interleaved backfill content has changed storage.
		if err := func() (retErr error) {
			leaseOwner, err := registry.BeginExternalContentMutation(ctx, site.Name)
			if err != nil {
				return err
			}
			defer func() {
				closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := registry.EndExternalContentMutation(closeCtx, site.Name, leaseOwner); err != nil {
					if retErr == nil {
						retErr = err
					} else {
						log.Printf("backfill-gallery: %s close content mutation: %v", site.Name, err)
					}
				}
			}()
			if plan.result.SpotJSONWritten {
				data, err := marshalSiteMetadataFile(plan.merged)
				if err != nil {
					return err
				}
				if err := s.sites.Put(ctx, site.Name, siteMetadataFileName, "application/json", data); err != nil {
					return fmt.Errorf("%s: write %s: %w", site.Name, siteMetadataFileName, err)
				}
			}
			if plan.result.ScreenshotWritten {
				if err := s.sites.Put(ctx, site.Name, "_screenshot.png", "image/png", screenshotData); err != nil {
					return fmt.Errorf("%s: write _screenshot.png: %w", site.Name, err)
				}
			}
			return nil
		}(); err != nil {
			return galleryBackfillSiteResult{}, err
		}
	}
	return plan.result, nil
}

func (s *Server) planGalleryBackfillSite(ctx context.Context, site SiteRecord, opts galleryBackfillOptions) (galleryBackfillSitePlan, error) {
	files, err := currentDeployMetadataFiles(ctx, s.sites, site.Name)
	if err != nil {
		return galleryBackfillSitePlan{}, fmt.Errorf("%s: read deployed files: %w", site.Name, err)
	}
	meta, err := metadataForDeploy(site.Name, files)
	if err != nil {
		return galleryBackfillSitePlan{}, fmt.Errorf("%s: %w", site.Name, err)
	}
	restricted, _, _ := s.policySummaryForSite(ctx, site.Name)
	if opts.AITags && !restricted && !meta.TagsSpecified && len(site.Tags) == 0 {
		meta.Tags = s.suggestSiteTags(ctx, site.Name, files, meta.SiteMetadata)
		meta.TagsSpecified = len(meta.Tags) > 0
	}
	merged := mergeBackfillMetadata(site, meta, opts.Force)
	spotJSONExists, err := siteFileExists(ctx, s.sites, site.Name, siteMetadataFileName)
	if err != nil {
		return galleryBackfillSitePlan{}, err
	}
	previewExists := s.hasSitePreview(ctx, site.Name)
	result := galleryBackfillSiteResult{
		MetadataUpdated: shouldUpdateBackfillMetadata(site, merged),
		SpotJSONWritten: opts.WriteSpotJSON && (opts.Force || !spotJSONExists),
	}
	if opts.Screenshots {
		if restricted {
			result.ScreenshotSkipped = true
		} else {
			result.ScreenshotWritten = opts.Force || !previewExists
		}
	}
	return galleryBackfillSitePlan{result: result, merged: merged, restricted: restricted}, nil
}

func currentDeployMetadataFiles(ctx context.Context, sites SiteStorage, site string) ([]deployFile, error) {
	paths, err := sites.List(ctx, site)
	if err != nil {
		return nil, err
	}
	files := make([]deployFile, 0, len(paths))
	for _, path := range paths {
		file := deployFile{path: path}
		if path == "index.html" || path == siteMetadataFileName {
			data, err := readSiteFile(ctx, sites, site, path)
			if err != nil {
				return nil, err
			}
			file.data = data
		}
		files = append(files, file)
	}
	return files, nil
}

func readSiteFile(ctx context.Context, sites SiteStorage, site, path string) ([]byte, error) {
	rc, _, err := sites.Open(ctx, site, path)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, maxDeploySize))
}

func mergeBackfillMetadata(site SiteRecord, meta deploySiteMetadata, force bool) SiteMetadata {
	next := resolveSiteMetadata(meta, site.Tags)
	if force {
		return next
	}
	if site.Title != "" {
		next.Title = site.Title
	}
	if site.Description != "" {
		next.Description = site.Description
	}
	if len(site.Tags) > 0 {
		next.Tags = cloneSiteTags(site.Tags)
	}
	return next
}

func shouldUpdateBackfillMetadata(site SiteRecord, meta SiteMetadata) bool {
	if site.Title != meta.Title || site.Description != meta.Description {
		return true
	}
	if len(site.Tags) != len(meta.Tags) {
		return true
	}
	for i := range site.Tags {
		if site.Tags[i] != meta.Tags[i] {
			return true
		}
	}
	return false
}

func marshalSiteMetadataFile(meta SiteMetadata) ([]byte, error) {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func siteFileExists(ctx context.Context, sites SiteStorage, site, path string) (bool, error) {
	rc, _, err := sites.Open(ctx, site, path)
	if err != nil {
		if errors.Is(err, ErrNotFound) || siteObjectNotFound(err) {
			return false, nil
		}
		return false, err
	}
	rc.Close()
	return true, nil
}

func captureBackfillScreenshot(ctx context.Context, opts galleryBackfillOptions, site, url string) ([]byte, error) {
	if opts.captureScreenshot != nil {
		return opts.captureScreenshot(ctx, site, url)
	}
	chrome := opts.Chrome
	if chrome == "" {
		var err error
		chrome, err = findChrome()
		if err != nil {
			return nil, err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tmp, err := os.CreateTemp("", "spot-screenshot-*.png")
	if err != nil {
		return nil, err
	}
	tmpName := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpName)
	profileDir, err := os.MkdirTemp("", "spot-chrome-profile-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(profileDir)
	args := []string{
		"--headless=new",
		"--disable-gpu",
		"--disable-dev-shm-usage",
		"--disable-crash-reporter",
		"--disable-crashpad",
		"--no-first-run",
		"--no-default-browser-check",
		"--no-sandbox",
		"--hide-scrollbars",
		"--ignore-certificate-errors",
		"--window-size=1280,800",
		"--user-data-dir=" + profileDir,
		"--screenshot=" + tmpName,
		url,
	}
	cmd := exec.CommandContext(ctx, chrome, args...)
	if os.Getenv("HOME") == "" {
		cmd.Env = append(os.Environ(),
			"HOME="+os.TempDir(),
			"XDG_CONFIG_HOME="+filepath.Join(os.TempDir(), ".config"),
		)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s failed: %w: %s", filepath.Base(chrome), err, strings.TrimSpace(string(out)))
	}
	data, err := os.ReadFile(tmpName)
	if err != nil {
		return nil, err
	}
	if !isPreviewImage(http.DetectContentType(data)) {
		return nil, fmt.Errorf("captured screenshot is not a supported image")
	}
	return data, nil
}

func findChrome() (string, error) {
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable"} {
		path, err := exec.LookPath(name)
		if err == nil {
			return path, nil
		}
	}
	return "", errors.New("chromium/google-chrome not found; pass -chrome or disable -screenshots")
}

func backfillSiteURL(scheme, domain, site string) string {
	scheme = strings.TrimSuffix(strings.TrimSpace(scheme), "://")
	if scheme == "" {
		scheme = "https"
	}
	host := strings.TrimSuffix(strings.TrimSpace(domain), ".")
	port := ""
	if h, p, err := net.SplitHostPort(host); err == nil {
		host = h
		port = p
	}
	host = site + "." + host
	if port != "" {
		host = net.JoinHostPort(host, port)
	}
	return scheme + "://" + host + "/"
}
