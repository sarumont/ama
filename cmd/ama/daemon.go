package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/sarumont/ama/config"
	"github.com/sarumont/ama/internal/bluray"
	"github.com/sarumont/ama/internal/cd"
	"github.com/sarumont/ama/internal/disc"
	"github.com/sarumont/ama/internal/identify"
	"github.com/sarumont/ama/internal/manifest"
	"github.com/sarumont/ama/internal/musicbrainz"
	"github.com/sarumont/ama/internal/radarr"
	"github.com/sarumont/ama/internal/subtitle"
	"github.com/sarumont/ama/internal/web"
)

// httpTimeout bounds every outbound HTTP call the daemon makes directly
// (TMDB, MusicBrainz, Radarr, Sonarr) — long enough for a slow connection,
// short enough that a stuck upstream cannot wedge the one-disc-at-a-time
// pipeline indefinitely.
const httpTimeout = 30 * time.Second

// staleAfterNotReady selects disc.New's own default tolerance for
// consecutive not-ready polls before it emits DriveStalled. There is no
// config knob for this (docs/CONFIG.md documents disc.poll_interval only).
const staleAfterNotReady = 0

// userAgent identifies AMA to MusicBrainz, whose API usage policy requires a
// descriptive User-Agent naming the application, a version, and a contact
// URL.
var userAgent = "ama/" + manifest.Version + " ( https://github.com/sarumont/ama )"

// daemon holds every long-lived client the rip pipeline drives, built once
// at startup from cfg. Only run (the pipeline loop) and the HTTP/disc
// goroutines it starts touch these concurrently; each client is either
// stateless per call or, like manifest.Update, safe for concurrent use on
// its own.
type daemon struct {
	cfg *config.Config
	log *slog.Logger

	web      *web.Server
	detector *disc.Detector

	bluray            *bluray.Client
	cd                *cd.Client
	tmdb              *identify.Client
	subtitleAnalyzer  *subtitle.Analyzer
	subtitleConverter *subtitle.Converter
	subtitleMuxer     *subtitle.Muxer
	// radarr is nil unless cfg.Radarr.Enabled. Sonarr is not constructed
	// here: TV/Sonarr integration is out of scope for v1 (docs/CLAUDE.md,
	// "Out of Scope"), and nothing in this package drives it, so a *Sonarr
	// client with nothing to call would be dead weight. config.Sonarr is
	// still validated the same way as config.Radarr, so the config surface
	// stays ready for when that work lands.
	radarr *radarr.Radarr
}

// newDaemon constructs every client from cfg but starts nothing — Start
// happens in run, once, so a construction failure (e.g. a bad web.Options)
// is reported before anything is listening or polling.
func newDaemon(cfg *config.Config, log *slog.Logger) (*daemon, error) {
	webServer, err := web.New(web.Options{Config: cfg, Logger: log})
	if err != nil {
		return nil, fmt.Errorf("web: %w", err)
	}

	httpClient := &http.Client{Timeout: httpTimeout}

	blurayClient := bluray.NewClient(cfg.MakeMKV.MinTrackDuration)
	blurayClient.Log = log

	mb := musicbrainz.New(userAgent, httpClient, "")
	cdClient := cd.NewClient(mb)
	cdClient.Log = log

	d := &daemon{
		cfg:              cfg,
		log:              log,
		web:              webServer,
		detector:         disc.New(cfg.Disc.Device, time.Duration(cfg.Disc.PollInterval)*time.Second, nil, staleAfterNotReady),
		bluray:           blurayClient,
		cd:               cdClient,
		tmdb:             identify.New(cfg.TMDB.APIKey, httpClient, ""),
		subtitleAnalyzer: &subtitle.Analyzer{Log: log},
		subtitleConverter: &subtitle.Converter{
			TempDir:   cfg.Output.Temp,
			Languages: cfg.Subtitle.OCRLanguages,
			Log:       log,
		},
		subtitleMuxer: &subtitle.Muxer{Log: log},
	}
	if cfg.Radarr.Enabled {
		d.radarr = radarr.NewRadarr(cfg.Radarr, httpClient)
	}
	return d, nil
}

// checkTools validates everything that can be checked once at startup
// instead of failing mid-rip: the MakeMKV license key is written to its
// settings file (only if configured — a CD-only deployment never needs
// one), the configured OCR languages are all bundled in the image, and the
// mkvmerge binary CheckTools needs is present and new enough. A missing
// makemkvcon/whipper/ffmpeg/pgsrip binary is deliberately NOT checked here:
// none of bluray.Client, cd.Client or subtitle.Converter expose a
// tool-presence check, so those surface as a normal pipeline error on the
// first disc that needs them instead.
func (d *daemon) checkTools(ctx context.Context) error {
	if d.cfg.MakeMKV.Key != "" {
		if err := bluray.WriteLicenseKey(d.cfg.MakeMKV.Key); err != nil {
			return fmt.Errorf("writing MakeMKV license key: %w", err)
		}
	}
	if err := subtitle.ValidateLanguages(d.cfg.Subtitle.OCRLanguages, nil); err != nil {
		return err
	}
	if err := d.subtitleConverter.CheckTools(); err != nil {
		return err
	}
	if err := d.subtitleMuxer.CheckTools(ctx); err != nil {
		return err
	}
	return nil
}

// run starts the web server, the disc detector, and the rip pipeline, and
// blocks until ctx is cancelled (SIGINT/SIGTERM), then waits for graceful
// shutdown of both goroutines before returning. A failure in either the web
// server or the pipeline loop itself is returned; a failure ripping one
// disc is not — see runPipeline and handleDisc, which keep the daemon alive
// through anything short of ctx cancellation.
func (d *daemon) run(ctx context.Context) error {
	var wg sync.WaitGroup
	var webErr error

	wg.Add(1)
	go func() {
		defer wg.Done()
		webErr = d.web.Start(ctx)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		d.detector.Run(ctx)
	}()

	select {
	case <-d.web.Ready():
	case <-ctx.Done():
	}
	d.log.Info("web server ready", "addr", d.web.Addr())

	d.runPipeline(ctx)

	wg.Wait()
	if webErr != nil {
		return fmt.Errorf("web server: %w", webErr)
	}
	return nil
}

// runPipeline consumes disc.Detector events one at a time — "no background
// queue, one disc at a time; the drive is the queue" (docs/CLAUDE.md) — until
// ctx is cancelled or the events channel closes (which only happens once
// Run itself has returned, i.e. after ctx is already done).
func (d *daemon) runPipeline(ctx context.Context) {
	events := d.detector.Events()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			d.handleEvent(ctx, ev)
		}
	}
}

func (d *daemon) handleEvent(ctx context.Context, ev disc.Event) {
	switch ev.Type {
	case disc.DiscInserted:
		d.log.Info("disc inserted", "device", ev.Device)
		d.handleDisc(ctx, ev.Device)
	case disc.DiscRemoved:
		d.log.Info("disc removed", "device", ev.Device)
	case disc.DriveError:
		d.log.Warn("drive error", "device", ev.Device, "err", ev.Err)
	case disc.DriveStalled:
		d.log.Warn("drive stalled", "device", ev.Device)
	default:
		d.log.Warn("unrecognized drive event", "type", ev.Type, "device", ev.Device)
	}
}

// handleDisc drives one disc through identification, ripping, and (for a
// Blu-ray) subtitle processing and *arr integration. Every error from here
// down is recorded on the manifest and logged, never returned or panicked:
// a failed disc must not kill the daemon (issue #13's AC).
func (d *daemon) handleDisc(ctx context.Context, device string) {
	mounted, err := disc.Detect(device, nil)
	if err != nil {
		d.log.Error("examining disc", "device", device, "err", err)
		return
	}
	defer func() {
		if rerr := mounted.Release(); rerr != nil {
			d.log.Warn("releasing disc mount", "device", device, "err", rerr)
		}
	}()

	switch mounted.Kind {
	case disc.KindBluRay:
		d.ripBluRay(ctx, device, mounted)
	case disc.KindCD:
		d.ripCD(ctx, device)
	case disc.KindDVD:
		d.log.Warn("DVD ripping is out of scope for v1, skipping", "device", device)
		d.ejectIfConfigured(ctx, device)
	default:
		d.log.Warn("disc is not a Blu-ray, DVD, or CD; skipping", "device", device)
		d.ejectIfConfigured(ctx, device)
	}
}

func (d *daemon) ejectIfConfigured(ctx context.Context, device string) {
	if !d.cfg.Disc.EjectOnComplete {
		return
	}
	if err := d.detector.Eject(ctx); err != nil {
		d.log.Warn("eject failed", "device", device, "err", err)
	}
}

// waitForConfirmation polls the manifest at path until its identification is
// confirmed (by the web UI's POST /confirm/:id, or by an earlier caller
// auto-confirming it directly) or ctx is cancelled. There is no daemon-side
// event source for a web request — the web layer and the pipeline share
// nothing but the manifest file on disk (see internal/web/handlers.go's own
// doc comment) — so polling the file it already reads and writes is the
// simplest thing that is actually correct.
func (d *daemon) waitForConfirmation(ctx context.Context, path string) (*manifest.Manifest, error) {
	const pollInterval = 2 * time.Second
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		m, err := manifest.Read(path)
		if err != nil {
			return nil, fmt.Errorf("reading manifest %s: %w", path, err)
		}
		if m.Identification.Confirmed {
			return m, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// relocate moves the manifest at oldPath to the canonical location
// manifest.Path/Dir now compute from m — which only resolves correctly once
// identification is confirmed and m.Identification.Title/Year (Blu-ray) or
// Artist/Album (CD) are set. Before confirmation, manifest.Dir falls back to
// m.ID for the directory name (see manifest.Dir's doc comment), so the
// manifest created at disc-insertion time necessarily starts somewhere
// manifest.Update's own path parameter does not follow automatically once
// those fields change. If the canonical path is unchanged (confirmation set
// no naming field, which should not normally happen) this is a no-op.
func relocate(root string, m *manifest.Manifest, oldPath string) (newPath string, err error) {
	newPath = manifest.Path(root, m)
	if newPath == oldPath {
		return oldPath, nil
	}
	if err := manifest.Write(newPath, m); err != nil {
		return "", fmt.Errorf("writing manifest at its confirmed location %s: %w", newPath, err)
	}
	oldDir := parentDir(oldPath)
	if err := removeFileAndEmptyDir(oldPath, oldDir); err != nil {
		// The manifest already exists (and is authoritative) at newPath, so
		// this is a leftover cleanup failure, not a lost manifest. Log and
		// continue rather than aborting the rip over it.
		return newPath, fmt.Errorf("cleaning up pre-confirmation manifest at %s: %w", oldPath, err)
	}
	return newPath, nil
}

func recordError(path string, log *slog.Logger, context string, err error) {
	log.Error(context, "err", err)
	if uerr := manifest.AddError(path, fmt.Sprintf("%s: %v", context, err)); uerr != nil {
		log.Error("recording error on manifest", "path", path, "err", uerr)
	}
}

func recordWarning(path string, log *slog.Logger, warning string) {
	log.Warn(warning, "path", path)
	if err := manifest.AddWarning(path, warning); err != nil {
		log.Error("recording warning on manifest", "path", path, "err", err)
	}
}
