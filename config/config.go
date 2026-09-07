// Package config loads AMA's YAML configuration file and applies environment
// variable overrides.
//
// Resolution order for the config file is: an explicit path passed to Load, the
// AMA_CONFIG environment variable, then ama.yaml in the working directory.
// Values are layered defaults -> YAML -> environment, so an environment
// variable always wins over the file.
//
// Load deliberately does not validate. Different run modes need different
// fields (a CD-only run never needs makemkv.key, for example), so callers
// decide what they require and may call Validate for the common checks.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultFileName is the config file looked up in the working directory when no
// explicit path and no AMA_CONFIG environment variable are given.
const DefaultFileName = "ama.yaml"

// envPrefix is prepended to every environment variable override.
const envPrefix = "AMA_"

// redacted replaces secret values in String output.
const redacted = "<redacted>"

// Config is the full AMA configuration.
type Config struct {
	MakeMKV  MakeMKV  `yaml:"makemkv"`
	TMDB     TMDB     `yaml:"tmdb"`
	Output   Output   `yaml:"output"`
	Radarr   Arr      `yaml:"radarr"`
	Sonarr   Arr      `yaml:"sonarr"`
	Web      Web      `yaml:"web"`
	Subtitle Subtitle `yaml:"subtitle"`
	Disc     Disc     `yaml:"disc"`
}

// MakeMKV configures the Blu-ray ripping backend.
type MakeMKV struct {
	// Key is the MakeMKV license key. Required to rip a Blu-ray, but not at
	// load time: a CD-only deployment never needs it.
	Key string `yaml:"key"`
	// MinTrackDuration is the duration in seconds below which a title is
	// skipped entirely.
	MinTrackDuration int `yaml:"min_track_duration"`
}

// TMDB configures movie identification.
type TMDB struct {
	// APIKey is the TMDB read access token, sent as a bearer token.
	APIKey string `yaml:"api_key"`
	// AutoConfirmThreshold is the fuzzy-match confidence in [0,1] at or above
	// which a candidate is auto-confirmed without a manual step. Nil means
	// unset: every disc requires manual confirmation in the web UI.
	AutoConfirmThreshold *float64 `yaml:"auto_confirm_threshold"`
}

// Output configures library and scratch paths.
type Output struct {
	Movies string `yaml:"movies"`
	Music  string `yaml:"music"`
	Temp   string `yaml:"temp"`
}

// Arr configures a Radarr or Sonarr instance. Both services share the same
// shape, so both the radarr and sonarr sections unmarshal into this type.
type Arr struct {
	Enabled bool   `yaml:"enabled"`
	URL     string `yaml:"url"`
	APIKey  string `yaml:"api_key"`
}

// Web configures the HTTP server.
type Web struct {
	Port int    `yaml:"port"`
	Host string `yaml:"host"`
}

// Subtitle configures subtitle analysis and OCR.
type Subtitle struct {
	// ForcedRatioThreshold is the smaller/larger size ratio below which an
	// English PGS track is flagged as a forced candidate.
	ForcedRatioThreshold float64 `yaml:"forced_ratio_threshold"`
	// OCRLanguages are tesseract language codes.
	OCRLanguages []string `yaml:"ocr_languages"`
}

// Disc configures optical drive polling.
type Disc struct {
	Device string `yaml:"device"`
	// PollInterval is the number of seconds between disc presence checks.
	PollInterval    int  `yaml:"poll_interval"`
	EjectOnComplete bool `yaml:"eject_on_complete"`
}

// Default returns the configuration with every documented default applied and
// no file or environment layered on top.
func Default() *Config {
	return &Config{
		MakeMKV: MakeMKV{
			MinTrackDuration: 60,
		},
		Output: Output{
			Movies: "/media/library/movies",
			Music:  "/media/library/music",
			Temp:   "/tmp/ama",
		},
		Radarr: Arr{
			Enabled: true,
			URL:     "http://radarr:7878",
		},
		Sonarr: Arr{
			Enabled: false,
			URL:     "http://sonarr:8989",
		},
		Web: Web{
			Port: 8080,
			Host: "0.0.0.0",
		},
		Subtitle: Subtitle{
			ForcedRatioThreshold: 0.25,
			OCRLanguages:         []string{"eng"},
		},
		Disc: Disc{
			Device:          "/dev/sr0",
			PollInterval:    5,
			EjectOnComplete: true,
		},
	}
}

// Load reads the configuration from path, or from the resolved default location
// when path is empty, and applies environment variable overrides.
//
// A missing file is only an error when the path was chosen explicitly (via the
// argument or AMA_CONFIG); a missing ama.yaml in the working directory yields
// defaults plus environment overrides.
func Load(path string) (*Config, error) {
	resolved, explicit := ResolvePath(path)

	cfg := Default()
	data, err := os.ReadFile(resolved)
	switch {
	case err == nil:
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("config: parsing %s: %w", resolved, err)
		}
	case errors.Is(err, os.ErrNotExist) && !explicit:
		// No config file in the working directory: defaults plus env is a
		// valid, fully container-driven configuration.
	default:
		return nil, fmt.Errorf("config: reading %s: %w", resolved, err)
	}

	if err := applyEnv(cfg, os.LookupEnv); err != nil {
		return nil, err
	}
	return cfg, nil
}

// ResolvePath reports the config file path Load will read and whether it was
// chosen explicitly (by argument or AMA_CONFIG) rather than defaulted.
func ResolvePath(path string) (resolved string, explicit bool) {
	if path != "" {
		return path, true
	}
	if env := os.Getenv("AMA_CONFIG"); env != "" {
		return env, true
	}
	return DefaultFileName, false
}

// Validate checks the constraints that apply to every run mode and returns all
// problems found, joined into a single error.
//
// makemkv.key is intentionally not checked: it is only needed once a Blu-ray is
// actually being ripped.
func (c *Config) Validate() error {
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	if c.TMDB.APIKey == "" {
		fail("tmdb.api_key is required")
	}
	if t := c.TMDB.AutoConfirmThreshold; t != nil && (*t < 0 || *t > 1) {
		fail("tmdb.auto_confirm_threshold must be between 0 and 1, got %v", *t)
	}
	if c.MakeMKV.MinTrackDuration < 0 {
		fail("makemkv.min_track_duration must not be negative, got %d", c.MakeMKV.MinTrackDuration)
	}
	if c.Output.Movies == "" {
		fail("output.movies is required")
	}
	if c.Output.Music == "" {
		fail("output.music is required")
	}
	if c.Output.Temp == "" {
		fail("output.temp is required")
	}
	validateArr("radarr", c.Radarr, fail)
	validateArr("sonarr", c.Sonarr, fail)
	if c.Web.Port < 1 || c.Web.Port > 65535 {
		fail("web.port must be between 1 and 65535, got %d", c.Web.Port)
	}
	if c.Subtitle.ForcedRatioThreshold <= 0 || c.Subtitle.ForcedRatioThreshold >= 1 {
		fail("subtitle.forced_ratio_threshold must be between 0 and 1 exclusive, got %v", c.Subtitle.ForcedRatioThreshold)
	}
	if c.Disc.PollInterval < 1 {
		fail("disc.poll_interval must be at least 1 second, got %d", c.Disc.PollInterval)
	}

	return errors.Join(errs...)
}

func validateArr(name string, arr Arr, fail func(string, ...any)) {
	if !arr.Enabled {
		return
	}
	if arr.URL == "" {
		fail("%s.url is required when %s.enabled is true", name, name)
	}
	if arr.APIKey == "" {
		fail("%s.api_key is required when %s.enabled is true", name, name)
	}
}

// String renders the configuration as YAML with every secret redacted, so it is
// safe to log.
func (c *Config) String() string {
	safe := *c
	safe.MakeMKV.Key = redact(safe.MakeMKV.Key)
	safe.TMDB.APIKey = redact(safe.TMDB.APIKey)
	safe.Radarr.APIKey = redact(safe.Radarr.APIKey)
	safe.Sonarr.APIKey = redact(safe.Sonarr.APIKey)

	out, err := yaml.Marshal(&safe)
	if err != nil {
		return fmt.Sprintf("config: unrenderable: %v", err)
	}
	return string(out)
}

func redact(s string) string {
	if s == "" {
		return ""
	}
	return redacted
}

// envBinding maps one config field to its environment variable overrides.
type envBinding struct {
	section string
	field   string
	apply   func(*Config, string) error
}

// names lists the accepted variable names, most preferred first.
//
// docs/CONFIG.md describes nesting with a double underscore but spells every
// example with a single one (AMA_MAKEMKV_KEY, AMA_OUTPUT_MOVIES). Both forms
// are accepted; the documented single-underscore spelling wins.
func (b envBinding) names() []string {
	return []string{
		envPrefix + b.section + "_" + b.field,
		envPrefix + b.section + "__" + b.field,
	}
}

func envBindings() []envBinding {
	return []envBinding{
		{"MAKEMKV", "KEY", func(c *Config, v string) error { c.MakeMKV.Key = v; return nil }},
		{"MAKEMKV", "MIN_TRACK_DURATION", func(c *Config, v string) error {
			return setInt(&c.MakeMKV.MinTrackDuration, v)
		}},
		{"TMDB", "API_KEY", func(c *Config, v string) error { c.TMDB.APIKey = v; return nil }},
		{"TMDB", "AUTO_CONFIRM_THRESHOLD", func(c *Config, v string) error {
			return setFloatPtr(&c.TMDB.AutoConfirmThreshold, v)
		}},
		{"OUTPUT", "MOVIES", func(c *Config, v string) error { c.Output.Movies = v; return nil }},
		{"OUTPUT", "MUSIC", func(c *Config, v string) error { c.Output.Music = v; return nil }},
		{"OUTPUT", "TEMP", func(c *Config, v string) error { c.Output.Temp = v; return nil }},
		{"RADARR", "ENABLED", func(c *Config, v string) error { return setBool(&c.Radarr.Enabled, v) }},
		{"RADARR", "URL", func(c *Config, v string) error { c.Radarr.URL = v; return nil }},
		{"RADARR", "API_KEY", func(c *Config, v string) error { c.Radarr.APIKey = v; return nil }},
		{"SONARR", "ENABLED", func(c *Config, v string) error { return setBool(&c.Sonarr.Enabled, v) }},
		{"SONARR", "URL", func(c *Config, v string) error { c.Sonarr.URL = v; return nil }},
		{"SONARR", "API_KEY", func(c *Config, v string) error { c.Sonarr.APIKey = v; return nil }},
		{"WEB", "PORT", func(c *Config, v string) error { return setInt(&c.Web.Port, v) }},
		{"WEB", "HOST", func(c *Config, v string) error { c.Web.Host = v; return nil }},
		{"SUBTITLE", "FORCED_RATIO_THRESHOLD", func(c *Config, v string) error {
			return setFloat(&c.Subtitle.ForcedRatioThreshold, v)
		}},
		{"SUBTITLE", "OCR_LANGUAGES", func(c *Config, v string) error {
			c.Subtitle.OCRLanguages = splitList(v)
			return nil
		}},
		{"DISC", "DEVICE", func(c *Config, v string) error { c.Disc.Device = v; return nil }},
		{"DISC", "POLL_INTERVAL", func(c *Config, v string) error { return setInt(&c.Disc.PollInterval, v) }},
		{"DISC", "EJECT_ON_COMPLETE", func(c *Config, v string) error {
			return setBool(&c.Disc.EjectOnComplete, v)
		}},
	}
}

// applyEnv overlays environment variables onto cfg. lookup is injected so tests
// do not have to mutate the process environment.
func applyEnv(cfg *Config, lookup func(string) (string, bool)) error {
	for _, b := range envBindings() {
		for _, name := range b.names() {
			value, ok := lookup(name)
			if !ok {
				continue
			}
			if err := b.apply(cfg, value); err != nil {
				return fmt.Errorf("config: %s: %w", name, err)
			}
			break
		}
	}
	return nil
}

func setInt(dst *int, v string) error {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return fmt.Errorf("expected an integer, got %q", v)
	}
	*dst = n
	return nil
}

func setFloat(dst *float64, v string) error {
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return fmt.Errorf("expected a number, got %q", v)
	}
	*dst = f
	return nil
}

// setFloatPtr treats an empty value as "unset", clearing the pointer.
func setFloatPtr(dst **float64, v string) error {
	if strings.TrimSpace(v) == "" {
		*dst = nil
		return nil
	}
	var f float64
	if err := setFloat(&f, v); err != nil {
		return err
	}
	*dst = &f
	return nil
}

func setBool(dst *bool, v string) error {
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return fmt.Errorf("expected a boolean, got %q", v)
	}
	*dst = b
	return nil
}

// splitList parses a comma-separated list, dropping empty entries.
func splitList(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
