package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/sarumont/ama/internal/bluray"
	"github.com/sarumont/ama/internal/disc"
	"github.com/sarumont/ama/internal/identify"
	"github.com/sarumont/ama/internal/manifest"
	"github.com/sarumont/ama/internal/musicbrainz"
	"github.com/sarumont/ama/internal/radarr"
	"github.com/sarumont/ama/internal/subtitle"
)

// ripBluRay drives docs/ARCHITECTURE.md's Blu-ray flow end to end: read
// BDMV metadata, rank TMDB candidates, wait for (or auto-apply) confirmation,
// rip, classify, analyze/OCR/mux subtitles on the main feature, notify
// Radarr, eject.
func (d *daemon) ripBluRay(ctx context.Context, device string, mounted disc.MountedDisc) {
	root := d.cfg.Output.Movies
	m := manifest.New(manifest.DiscTypeBluRay, device)
	m.Identification.Method = "bdmv+tmdb"

	discInfo, infoErr := bluray.ReadDiscInfo(mounted.Root)
	var label string
	var year int
	if discInfo != nil {
		label = discInfo.Title
		if discInfo.Year != nil {
			year = *discInfo.Year
		}
	}
	if label != "" {
		l := label
		m.Disc.Label = &l
	}

	path := manifest.Path(root, m)
	if err := manifest.Write(path, m); err != nil {
		d.log.Error("writing initial manifest", "device", device, "err", err)
		return
	}
	d.log.Info("blu-ray disc detected", "device", device, "label", label, "manifest", path)

	switch {
	case infoErr != nil && errors.Is(infoErr, bluray.ErrNoMetadata):
		recordWarning(path, d.log, fmt.Sprintf("no BDMV metadata found: %v", infoErr))
	case infoErr != nil:
		recordError(path, d.log, "reading BDMV metadata", infoErr)
	}

	var ranked []identify.RankedCandidate
	if label != "" {
		query, labelYear := identify.Normalize(label)
		if labelYear != 0 {
			year = labelYear
		}
		candidates, err := d.tmdb.Search(ctx, query, year)
		if err != nil {
			recordWarning(path, d.log, fmt.Sprintf("tmdb search failed: %v", err))
		} else {
			ranked = identify.Rank(label, candidates)
		}
	}
	top := identify.TopN(ranked, 3)

	err := manifest.Update(path, func(mm *manifest.Manifest) error {
		mm.Identification.Candidates = candidatesToManifest(top)
		if len(top) > 0 && identify.ShouldAutoConfirm(ranked, d.cfg.TMDB.AutoConfirmThreshold) {
			best := top[0]
			tmdbID := best.TMDBID
			mm.Identification.TMDBID = &tmdbID
			mm.Identification.Title = best.Title
			mm.Identification.Year = best.Year
			if best.Score > 0 {
				score := best.Score
				mm.Identification.Confidence = &score
			}
			mm.Identification.Confirmed = true
			now := time.Now().UTC()
			mm.Identification.ConfirmedAt = &now
			mm.Identification.ConfirmedBy = "auto"
			mm.Status = manifest.StatusRipping
		}
		return nil
	})
	if err != nil {
		recordError(path, d.log, "recording identification candidates", err)
		return
	}

	m, err = d.waitForConfirmation(ctx, path)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		recordError(path, d.log, "waiting for identification confirmation", err)
		return
	}

	path, err = relocate(root, m, path)
	if err != nil {
		recordError(path, d.log, "relocating manifest after confirmation", err)
		return
	}

	source := "dev:" + device
	allTracks, err := d.bluray.Info(ctx, source)
	if err != nil {
		recordError(path, d.log, "reading title info", err)
		return
	}
	classifications := bluray.Classify(allTracks, d.cfg.MakeMKV.MinTrackDuration)
	classByIndex := make(map[int]bluray.Classification, len(classifications))
	for _, c := range classifications {
		classByIndex[c.Track.Index] = c
	}

	if err := manifest.SetStatus(path, manifest.StatusRipping); err != nil {
		d.log.Error("setting status to ripping", "path", path, "err", err)
	}

	stagingDir := filepath.Join(d.cfg.Output.Temp, m.ID)
	rippedTracks, err := d.bluray.Rip(ctx, source, stagingDir)
	if err != nil {
		recordError(path, d.log, "ripping", err)
		return
	}
	defer func() { _ = os.RemoveAll(stagingDir) }()

	dir := manifest.Dir(root, m)
	base := filepath.Base(dir)

	// makemkvcon's --minlength drops every title under the threshold before
	// assigning indices to whatever remains, so Rip()'s track indices are
	// compacted relative to Info()'s absolute ones whenever any title on the
	// disc falls short of makemkv.min_track_duration — the common case (menu
	// loops, transitions). classByIndex is keyed by Info()'s absolute
	// indices, so it cannot be looked up with Rip()'s directly; ripToSource
	// reverses the compaction. See ripToSourceIndex's doc comment.
	ripToSource := ripToSourceIndex(allTracks, d.cfg.MakeMKV.MinTrackDuration)

	var manifestTracks []manifest.Track
	var mainFeaturePath, mainFeatureRel string
	rippedIndex := make(map[int]bool, len(rippedTracks))
	for _, t := range rippedTracks {
		sourceIndex, ok := ripToSource[t.Index]
		if !ok {
			recordWarning(path, d.log, fmt.Sprintf("ripped title index %d has no corresponding source title, skipping", t.Index))
			continue
		}
		rippedIndex[sourceIndex] = true
		cls, ok := classByIndex[sourceIndex]
		if !ok {
			recordWarning(path, d.log, fmt.Sprintf("ripped title index %d (source title %d) has no classification, skipping", t.Index, sourceIndex))
			continue
		}

		relName := outputFileName(base, sourceIndex, cls)
		dest := filepath.Join(dir, relName)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			recordError(path, d.log, fmt.Sprintf("creating output directory for track %d", sourceIndex), err)
			continue
		}
		if err := moveFile(t.OutputPath, dest); err != nil {
			recordError(path, d.log, fmt.Sprintf("moving ripped track %d into place", sourceIndex), err)
			continue
		}

		var edition *string
		if cls.Edition != "" {
			e := cls.Edition
			edition = &e
		}
		index, size, audioCount, chapters := sourceIndex, t.SizeBytes, t.AudioTrackCount, t.ChapterCount
		manifestTracks = append(manifestTracks, manifest.Track{
			MakeMKVIndex:    &index,
			SizeBytes:       &size,
			AudioTrackCount: &audioCount,
			ChapterCount:    &chapters,
			Role:            manifest.Role(cls.Role),
			RoleReason:      cls.Reason,
			Edition:         edition,
			DurationSeconds: t.DurationSeconds,
			OutputFile:      relName,
		})
		if cls.Role == bluray.RoleFeature {
			mainFeaturePath, mainFeatureRel = dest, relName
		}
	}

	// A classified, non-skip title that MakeMKV never wrote to disk: the
	// commentary-below-minimum precedence rule (bluray.Classify's doc
	// comment) can name a title "commentary" specifically to rescue it from
	// "skip", but Rip's own --minlength still drops anything under
	// makemkv.min_track_duration unconditionally. See this PR's description
	// for the tradeoff; flagging it here rather than silently losing the
	// track from the manifest.
	for _, cls := range classifications {
		if cls.Role == bluray.RoleSkip || rippedIndex[cls.Track.Index] {
			continue
		}
		recordWarning(path, d.log, fmt.Sprintf(
			"title %d classified as %q but makemkv.min_track_duration kept it from being ripped",
			cls.Track.Index, cls.Role))
	}

	err = manifest.Update(path, func(mm *manifest.Manifest) error {
		mm.Tracks = manifestTracks
		mm.Output.Path = dir + string(filepath.Separator)
		mm.Output.MainFeature = mainFeatureRel
		mm.Status = manifest.StatusAnalyzing
		return nil
	})
	if err != nil {
		recordError(path, d.log, "recording ripped tracks", err)
		return
	}

	if mainFeaturePath == "" {
		recordWarning(path, d.log, "no main feature was ripped; skipping subtitle analysis")
	} else {
		d.processSubtitles(ctx, path, mainFeaturePath)
	}

	d.notifyRadarr(ctx, path)
	d.finish(path)
	d.ejectIfConfigured(ctx, device)
}

// processSubtitles runs analyze -> forced detection -> OCR -> mux on
// mkvPath, updating the manifest at each stage. Per-stream OCR failures are
// isolated by subtitle.Converter.Convert itself and do not abort the rip;
// only a failure of the analysis step itself (ffprobe unusable) or the mux
// step is recorded as a manifest error.
func (d *daemon) processSubtitles(ctx context.Context, manifestPath, mkvPath string) {
	streams, err := d.subtitleAnalyzer.Analyze(ctx, mkvPath)
	if err != nil {
		recordError(manifestPath, d.log, "analyzing subtitles", err)
		return
	}
	streams = subtitle.DetectForcedCandidates(streams, d.cfg.Subtitle.ForcedRatioThreshold)
	if err := subtitle.WriteSidecar(mkvPath, streams); err != nil {
		recordWarning(manifestPath, d.log, fmt.Sprintf("writing subtitle sidecar: %v", err))
	}

	err = manifest.Update(manifestPath, func(mm *manifest.Manifest) error {
		mm.Subtitles = subtitlesToManifest(streams)
		mm.Status = manifest.StatusPendingOCR
		return nil
	})
	if err != nil {
		recordError(manifestPath, d.log, "recording subtitle analysis", err)
		return
	}

	results, err := d.subtitleConverter.Convert(ctx, mkvPath, streams)
	if err != nil {
		recordError(manifestPath, d.log, "OCR conversion", err)
		return
	}

	processedPath, muxErr := d.subtitleMuxer.Mux(ctx, mkvPath, streams, results)
	if muxErr != nil {
		recordError(manifestPath, d.log, "muxing subtitles", muxErr)
	}

	err = manifest.Update(manifestPath, func(mm *manifest.Manifest) error {
		mm.Subtitles = subtitlesToManifest(streams)
		if processedPath != "" {
			mm.Output.ProcessedFeature = filepath.Base(processedPath)
		}
		return nil
	})
	if err != nil {
		recordError(manifestPath, d.log, "recording subtitle conversion", err)
	}
}

// notifyRadarr adds the identified movie to Radarr and triggers an import
// scan of its output directory. A failure here is a warning, not a manifest
// error: the rip itself already succeeded, and docs/CLAUDE.md's Radarr step
// is the last, optional link in the chain.
func (d *daemon) notifyRadarr(ctx context.Context, manifestPath string) {
	if d.radarr == nil {
		return
	}
	m, err := manifest.Read(manifestPath)
	if err != nil {
		d.log.Error("reading manifest for radarr", "path", manifestPath, "err", err)
		return
	}
	if m.Identification.TMDBID == nil {
		return
	}

	result, err := d.radarr.AddMovie(ctx, *m.Identification.TMDBID, radarr.AddOptions{
		RootFolderPath:    d.cfg.Output.Movies,
		QualityProfileID:  d.cfg.Radarr.QualityProfileID,
		LanguageProfileID: d.cfg.Radarr.LanguageProfileID,
	})
	if err != nil {
		recordWarning(manifestPath, d.log, fmt.Sprintf("radarr add movie failed: %v", err))
		return
	}

	scanPath := manifest.Dir(d.cfg.Output.Movies, m)
	scanErr := d.radarr.TriggerImportScan(ctx, scanPath)

	now := time.Now().UTC()
	err = manifest.Update(manifestPath, func(mm *manifest.Manifest) error {
		if mm.Radarr == nil {
			mm.Radarr = &manifest.Arr{}
		}
		mm.Radarr.Added = result.Added || result.AlreadyExists
		mm.Radarr.AddedAt = &now
		if scanErr == nil {
			mm.Radarr.ImportTriggered = true
			mm.Radarr.ImportTriggeredAt = &now
		}
		return nil
	})
	if err != nil {
		d.log.Error("recording radarr status", "path", manifestPath, "err", err)
	}
	if scanErr != nil {
		recordWarning(manifestPath, d.log, fmt.Sprintf("radarr import scan trigger failed: %v", scanErr))
	}
}

// finish marks the manifest complete, unless something already marked it
// error — a rip that failed partway through must not have that overwritten
// by a blanket "it's done" at the end of the pipeline. The check and the
// write happen inside one Update call (rather than a preceding Read plus a
// separate Update) so a concurrent web-UI write to the same manifest cannot
// land in between them and get silently clobbered: manifest.Update takes
// this path's lock for the whole read-modify-write, but a check made before
// calling it is not covered by that lock.
func (d *daemon) finish(manifestPath string) {
	now := time.Now().UTC()
	if err := manifest.Update(manifestPath, func(mm *manifest.Manifest) error {
		if mm.Status == manifest.StatusError {
			return nil
		}
		mm.Status = manifest.StatusComplete
		mm.RippedAt = now
		return nil
	}); err != nil {
		d.log.Error("marking manifest complete", "path", manifestPath, "err", err)
	}
}

// ripCD drives docs/ARCHITECTURE.md's CD flow: read the disc ID and rank
// MusicBrainz candidates, wait for confirmation, rip with AccurateRip
// verification, eject. CDs have no subtitle or *arr steps.
func (d *daemon) ripCD(ctx context.Context, device string) {
	root := d.cfg.Output.Music
	m := manifest.New(manifest.DiscTypeCD, device)
	m.Identification.Method = "musicbrainz"

	path := manifest.Path(root, m)
	if err := manifest.Write(path, m); err != nil {
		d.log.Error("writing initial manifest", "device", device, "err", err)
		return
	}
	d.log.Info("CD detected", "device", device, "manifest", path)

	lookup, err := d.cd.Identify(ctx, device)
	if err != nil {
		recordError(path, d.log, "identifying CD", err)
		return
	}

	err = manifest.Update(path, func(mm *manifest.Manifest) error {
		mm.Identification.Candidates = mbReleasesToManifest(lookup.Releases)
		return nil
	})
	if err != nil {
		recordError(path, d.log, "recording CD candidates", err)
		return
	}
	if len(lookup.Releases) == 0 {
		recordWarning(path, d.log, fmt.Sprintf(
			"no MusicBrainz release found for disc id %s; manual entry required", lookup.DiscID))
	}

	m, err = d.waitForConfirmation(ctx, path)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		recordError(path, d.log, "waiting for identification confirmation", err)
		return
	}

	path, err = relocate(root, m, path)
	if err != nil {
		recordError(path, d.log, "relocating manifest after confirmation", err)
		return
	}

	// The confirmed release must be one of the candidates Identify actually
	// found: cd.Client.Rip pins -R to a real MusicBrainz release ID by
	// design (the issue #9 spike's non-interactive, no -U approach), and a
	// manual/unmatched confirmation carries no such ID. See this PR's
	// description: manual CD entry with no matched release cannot be
	// ripped yet.
	release, ok := findRelease(lookup.Releases, m.Identification.MBReleaseID)
	if !ok {
		recordError(path, d.log, "starting rip", fmt.Errorf(
			"no MusicBrainz release matches mb_release_id %q; manual (unmatched) CD identification cannot be ripped",
			m.Identification.MBReleaseID))
		return
	}

	if err := manifest.SetStatus(path, manifest.StatusRipping); err != nil {
		d.log.Error("setting status to ripping", "path", path, "err", err)
	}

	result, err := d.cd.Rip(ctx, device, release, root)
	if err != nil {
		recordError(path, d.log, "ripping CD", err)
		return
	}

	tracks := make([]manifest.Track, 0, len(result.Tracks))
	for _, t := range result.Tracks {
		title, duration := trackInfoFor(release, t.Number)
		verified := t.Verified()
		number := t.Number
		tracks = append(tracks, manifest.Track{
			Number:          &number,
			Title:           title,
			AccurateRip:     &verified,
			DurationSeconds: duration,
			OutputFile:      relativeToRoot(root, t.Filename),
		})
		if !verified {
			recordWarning(path, d.log, fmt.Sprintf(
				"track %d not verified by AccurateRip (%s)", t.Number, result.AccurateRipSummary))
		}
	}

	err = manifest.Update(path, func(mm *manifest.Manifest) error {
		mm.Tracks = tracks
		mm.Output.Path = manifest.Dir(root, mm) + string(filepath.Separator)
		return nil
	})
	if err != nil {
		recordError(path, d.log, "recording ripped tracks", err)
		return
	}

	d.finish(path)
	d.ejectIfConfigured(ctx, device)
}

func candidatesToManifest(ranked []identify.RankedCandidate) []manifest.Candidate {
	out := make([]manifest.Candidate, 0, len(ranked))
	for _, r := range ranked {
		out = append(out, manifest.Candidate{
			TMDBID: r.TMDBID,
			Title:  r.Title,
			Year:   r.Year,
			Score:  r.Score,
		})
	}
	return out
}

func mbReleasesToManifest(releases []musicbrainz.Release) []manifest.Candidate {
	out := make([]manifest.Candidate, 0, len(releases))
	for _, r := range releases {
		out = append(out, manifest.Candidate{
			MBReleaseID:      r.MBReleaseID,
			MBReleaseGroupID: r.MBReleaseGroupID,
			Artist:           r.Artist,
			Album:            r.Album,
			Year:             r.Year,
		})
	}
	return out
}

func findRelease(releases []musicbrainz.Release, mbReleaseID string) (musicbrainz.Release, bool) {
	if mbReleaseID == "" {
		return musicbrainz.Release{}, false
	}
	for _, r := range releases {
		if r.MBReleaseID == mbReleaseID {
			return r, true
		}
	}
	return musicbrainz.Release{}, false
}

func trackInfoFor(release musicbrainz.Release, number int) (title string, durationSeconds int) {
	for _, t := range release.Tracks {
		if t.Number == number {
			return t.Title, t.DurationSeconds
		}
	}
	return "", 0
}

func subtitlesToManifest(streams []subtitle.SubtitleStream) []manifest.Subtitle {
	out := make([]manifest.Subtitle, 0, len(streams))
	for _, s := range streams {
		out = append(out, manifest.Subtitle{
			StreamIndex:           s.StreamIndex,
			Codec:                 s.Codec,
			Language:              s.Language,
			SizeBytes:             s.SizeBytes,
			ForcedFlagInSource:    s.ForcedFlagInSource,
			DefaultFlagInSource:   s.DefaultFlagInSource,
			HearingImpaired:       s.HearingImpaired,
			ForcedCandidate:       s.ForcedCandidate,
			ForcedCandidateReason: s.ForcedCandidateReason,
			PairedWithStreamIndex: s.PairedWithStreamIndex,
			NeedsOCR:              s.NeedsOCR,
			Converted:             s.Converted,
			ConversionError:       s.ConversionError,
		})
	}
	return out
}

// ripToSourceIndex maps a title index as Rip() (and thus rippedTracks)
// reports it back to the absolute index Info() (and thus classByIndex) uses.
// MakeMKV's --minlength option filters out titles under the threshold
// *before* numbering whatever remains — confirmed against MakeMKV's own
// support forum ("filtering out titles with a smaller length happens before
// numbering the titles"; e.g. with --minlength=1800, "Title #5 becomes
// title id 0 and Title #12 becomes title id 1") — so Rip()'s indices are
// compacted relative to Info()'s absolute ones whenever any title falls
// under minTrackDuration, not just reused as-is. That filter looks only at
// raw duration, with no notion of "commentary" (bluray.Classify's exemption
// that keeps a short commentary track classified "commentary" rather than
// "skip" does not save it from makemkvcon's own --minlength), so the
// reconstruction here must do the same: filter purely on duration, not on
// the classified role.
//
// allTracks must be in ascending Index order, which is what Client.Info
// returns (collectTracks sorts by index) — the order --minlength's own
// compaction preserves for whatever survives it.
func ripToSourceIndex(allTracks []bluray.Track, minTrackDuration int) map[int]int {
	out := make(map[int]int)
	next := 0
	for _, t := range allTracks {
		if t.DurationSeconds < minTrackDuration {
			continue
		}
		out[next] = t.Index
		next++
	}
	return out
}

// outputFileName names a ripped track's final file per docs/MANIFEST.md's
// two documented examples: the feature is "{base}.mkv" in the rip root, an
// alternate cut gets Plex/Jellyfin edition naming in the root, and anything
// else (extra, commentary — the two roles Role.Subdirectory routes to
// extras/) is "extras/{base}_t{NN}.mkv". This convention exists only as
// these two examples in docs/MANIFEST.md; it is not extracted into a shared,
// independently tested helper anywhere else in the codebase.
func outputFileName(base string, index int, cls bluray.Classification) string {
	switch cls.Role {
	case bluray.RoleFeature:
		return base + ".mkv"
	case bluray.RoleAlternateCut:
		return fmt.Sprintf("%s {edition-%s}.mkv", base, cls.Edition)
	default:
		return filepath.Join("extras", fmt.Sprintf("%s_t%02d.mkv", base, index))
	}
}

// relativeToRoot returns path relative to root, or just its base name if it
// is not actually under root (should not happen in practice, but a
// misconfigured whipper output template must not crash the manifest write).
func relativeToRoot(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil || len(rel) >= 2 && rel[:2] == ".." {
		return filepath.Base(path)
	}
	return rel
}

// moveFile moves src to dst, falling back to copy-then-remove when they are
// on different filesystems (os.Rename's EXDEV) — expected to be rare, since
// docs/CONFIG.md's compose example deliberately mounts output.temp on the
// same filesystem as the library specifically so this finalizing move is a
// cheap rename, not a multi-GB copy.
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("moving %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()

	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("moving %s: %w", src, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("moving %s: %w", src, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("moving %s: %w", src, err)
	}
	if err := os.Remove(src); err != nil {
		return fmt.Errorf("moving %s: %w", src, err)
	}
	return nil
}
