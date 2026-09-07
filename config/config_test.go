package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fullYAML mirrors the example in docs/CONFIG.md with every value changed away
// from its default, so a parse test proves each key is wired up.
const fullYAML = `
makemkv:
  key: mk-license
  min_track_duration: 90

tmdb:
  api_key: tmdb-token
  auto_confirm_threshold: 0.85

output:
  movies: /srv/movies
  music: /srv/music
  temp: /var/tmp/ama

radarr:
  enabled: false
  url: http://radarr.local:7878
  api_key: radarr-key

sonarr:
  enabled: true
  url: http://sonarr.local:8989
  api_key: sonarr-key

web:
  port: 9090
  host: 127.0.0.1

subtitle:
  forced_ratio_threshold: 0.4
  ocr_languages:
    - eng
    - fra

disc:
  device: /dev/sr1
  poll_interval: 30
  eject_on_complete: false
`

// writeConfig writes contents to a temp file and returns its path.
func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ama.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return path
}

func floatPtr(f float64) *float64 { return &f }

func TestLoadYAML(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		want    func() *Config
		wantErr string
	}{
		{
			name: "empty file yields documented defaults",
			yaml: "",
			want: Default,
		},
		{
			name: "full file overrides every key",
			yaml: fullYAML,
			want: func() *Config {
				return &Config{
					MakeMKV: MakeMKV{Key: "mk-license", MinTrackDuration: 90},
					TMDB:    TMDB{APIKey: "tmdb-token", AutoConfirmThreshold: floatPtr(0.85)},
					Output:  Output{Movies: "/srv/movies", Music: "/srv/music", Temp: "/var/tmp/ama"},
					Radarr:  Arr{Enabled: false, URL: "http://radarr.local:7878", APIKey: "radarr-key"},
					Sonarr:  Arr{Enabled: true, URL: "http://sonarr.local:8989", APIKey: "sonarr-key"},
					Web:     Web{Port: 9090, Host: "127.0.0.1"},
					Subtitle: Subtitle{
						ForcedRatioThreshold: 0.4,
						OCRLanguages:         []string{"eng", "fra"},
					},
					Disc: Disc{Device: "/dev/sr1", PollInterval: 30, EjectOnComplete: false},
				}
			},
		},
		{
			name: "partial file keeps defaults for untouched sections",
			yaml: "web:\n  port: 1234\n",
			want: func() *Config {
				c := Default()
				c.Web.Port = 1234
				return c
			},
		},
		{
			name: "false booleans in file override true defaults",
			yaml: "disc:\n  eject_on_complete: false\nradarr:\n  enabled: false\n",
			want: func() *Config {
				c := Default()
				c.Disc.EjectOnComplete = false
				c.Radarr.Enabled = false
				return c
			},
		},
		{
			name:    "malformed yaml is an error",
			yaml:    "web:\n\tport: nope\n",
			wantErr: "parsing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Ensure no ambient AMA_ variables leak into the result.
			clearAMAEnv(t)

			got, err := Load(writeConfig(t, tt.yaml))
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %q does not contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if want := tt.want(); !reflect.DeepEqual(got, want) {
				t.Errorf("config mismatch\n got: %+v\nwant: %+v", got, want)
			}
		})
	}
}

func TestAutoConfirmThreshold(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		env  map[string]string
		want *float64
	}{
		{
			name: "absent from file stays nil",
			yaml: "tmdb:\n  api_key: k\n",
			want: nil,
		},
		{
			name: "explicit null stays nil",
			yaml: "tmdb:\n  auto_confirm_threshold: null\n",
			want: nil,
		},
		{
			name: "set in file",
			yaml: "tmdb:\n  auto_confirm_threshold: 0.9\n",
			want: floatPtr(0.9),
		},
		{
			name: "zero is a real value, not unset",
			yaml: "tmdb:\n  auto_confirm_threshold: 0\n",
			want: floatPtr(0),
		},
		{
			name: "env sets it when file omits it",
			yaml: "",
			env:  map[string]string{"AMA_TMDB_AUTO_CONFIRM_THRESHOLD": "0.75"},
			want: floatPtr(0.75),
		},
		{
			name: "empty env value clears a file value",
			yaml: "tmdb:\n  auto_confirm_threshold: 0.9\n",
			env:  map[string]string{"AMA_TMDB_AUTO_CONFIRM_THRESHOLD": ""},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearAMAEnv(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			got, err := Load(writeConfig(t, tt.yaml))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			switch {
			case tt.want == nil && got.TMDB.AutoConfirmThreshold != nil:
				t.Fatalf("expected nil threshold, got %v", *got.TMDB.AutoConfirmThreshold)
			case tt.want != nil && got.TMDB.AutoConfirmThreshold == nil:
				t.Fatalf("expected threshold %v, got nil", *tt.want)
			case tt.want != nil && *got.TMDB.AutoConfirmThreshold != *tt.want:
				t.Fatalf("expected threshold %v, got %v", *tt.want, *got.TMDB.AutoConfirmThreshold)
			}
		})
	}
}

func TestEnvOverrides(t *testing.T) {
	// Every case starts from the full YAML file so a passing case proves the
	// environment beats an explicitly-set file value.
	tests := []struct {
		name    string
		env     map[string]string
		check   func(*testing.T, *Config)
		wantErr string
	}{
		{
			name: "string",
			env:  map[string]string{"AMA_MAKEMKV_KEY": "from-env"},
			check: func(t *testing.T, c *Config) {
				if c.MakeMKV.Key != "from-env" {
					t.Errorf("makemkv.key = %q", c.MakeMKV.Key)
				}
			},
		},
		{
			name: "int",
			env:  map[string]string{"AMA_WEB_PORT": "7777"},
			check: func(t *testing.T, c *Config) {
				if c.Web.Port != 7777 {
					t.Errorf("web.port = %d", c.Web.Port)
				}
			},
		},
		{
			name: "float",
			env:  map[string]string{"AMA_SUBTITLE_FORCED_RATIO_THRESHOLD": "0.1"},
			check: func(t *testing.T, c *Config) {
				if c.Subtitle.ForcedRatioThreshold != 0.1 {
					t.Errorf("subtitle.forced_ratio_threshold = %v", c.Subtitle.ForcedRatioThreshold)
				}
			},
		},
		{
			name: "bool",
			env:  map[string]string{"AMA_DISC_EJECT_ON_COMPLETE": "true"},
			check: func(t *testing.T, c *Config) {
				if !c.Disc.EjectOnComplete {
					t.Error("disc.eject_on_complete = false, want true")
				}
			},
		},
		{
			name: "string slice is comma separated",
			env:  map[string]string{"AMA_SUBTITLE_OCR_LANGUAGES": "eng, deu ,jpn"},
			check: func(t *testing.T, c *Config) {
				want := []string{"eng", "deu", "jpn"}
				if !reflect.DeepEqual(c.Subtitle.OCRLanguages, want) {
					t.Errorf("subtitle.ocr_languages = %v, want %v", c.Subtitle.OCRLanguages, want)
				}
			},
		},
		{
			name: "documented example set",
			env: map[string]string{
				"AMA_TMDB_API_KEY":    "tmdb-env",
				"AMA_RADARR_API_KEY":  "radarr-env",
				"AMA_OUTPUT_MOVIES":   "/media/movies",
				"AMA_DISC_DEVICE":     "/dev/sr9",
				"AMA_SONARR_ENABLED":  "true",
				"AMA_DISC_POLL_INTER": "ignored: not a real key",
			},
			check: func(t *testing.T, c *Config) {
				if c.TMDB.APIKey != "tmdb-env" {
					t.Errorf("tmdb.api_key = %q", c.TMDB.APIKey)
				}
				if c.Radarr.APIKey != "radarr-env" {
					t.Errorf("radarr.api_key = %q", c.Radarr.APIKey)
				}
				if c.Output.Movies != "/media/movies" {
					t.Errorf("output.movies = %q", c.Output.Movies)
				}
				if c.Disc.Device != "/dev/sr9" {
					t.Errorf("disc.device = %q", c.Disc.Device)
				}
				if !c.Sonarr.Enabled {
					t.Error("sonarr.enabled = false, want true")
				}
				// Unknown variables are ignored, not fatal.
				if c.Disc.PollInterval != 30 {
					t.Errorf("disc.poll_interval = %d, want the file value 30", c.Disc.PollInterval)
				}
			},
		},
		{
			name: "double underscore nesting is also accepted",
			env:  map[string]string{"AMA_OUTPUT__MUSIC": "/media/music"},
			check: func(t *testing.T, c *Config) {
				if c.Output.Music != "/media/music" {
					t.Errorf("output.music = %q", c.Output.Music)
				}
			},
		},
		{
			name: "single underscore wins over double",
			env: map[string]string{
				"AMA_WEB_HOST":  "single",
				"AMA_WEB__HOST": "double",
			},
			check: func(t *testing.T, c *Config) {
				if c.Web.Host != "single" {
					t.Errorf("web.host = %q, want %q", c.Web.Host, "single")
				}
			},
		},
		{
			name:    "unparseable int is an error",
			env:     map[string]string{"AMA_WEB_PORT": "eighty-eighty"},
			wantErr: "AMA_WEB_PORT",
		},
		{
			name:    "unparseable bool is an error",
			env:     map[string]string{"AMA_RADARR_ENABLED": "yes-please"},
			wantErr: "AMA_RADARR_ENABLED",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearAMAEnv(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			got, err := Load(writeConfig(t, fullYAML))
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %q does not contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			tt.check(t, got)
		})
	}
}

func TestEnvBeatsDefaultsWithoutFile(t *testing.T) {
	clearAMAEnv(t)
	t.Setenv("AMA_WEB_PORT", "9999")
	t.Chdir(t.TempDir()) // no ama.yaml here

	got, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Web.Port != 9999 {
		t.Errorf("web.port = %d, want 9999", got.Web.Port)
	}
	if got.Disc.Device != "/dev/sr0" {
		t.Errorf("disc.device = %q, want the default", got.Disc.Device)
	}
}

func TestResolvePath(t *testing.T) {
	tests := []struct {
		name         string
		arg          string
		amaConfig    string
		wantPath     string
		wantExplicit bool
	}{
		{
			name:         "explicit argument wins",
			arg:          "/etc/ama/custom.yaml",
			amaConfig:    "/config/ama.yaml",
			wantPath:     "/etc/ama/custom.yaml",
			wantExplicit: true,
		},
		{
			name:         "AMA_CONFIG when no argument",
			amaConfig:    "/config/ama.yaml",
			wantPath:     "/config/ama.yaml",
			wantExplicit: true,
		},
		{
			name:         "working directory default",
			wantPath:     DefaultFileName,
			wantExplicit: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearAMAEnv(t)
			if tt.amaConfig != "" {
				t.Setenv("AMA_CONFIG", tt.amaConfig)
			}

			path, explicit := ResolvePath(tt.arg)
			if path != tt.wantPath || explicit != tt.wantExplicit {
				t.Errorf("ResolvePath(%q) = (%q, %v), want (%q, %v)",
					tt.arg, path, explicit, tt.wantPath, tt.wantExplicit)
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	clearAMAEnv(t)
	missing := filepath.Join(t.TempDir(), "nope.yaml")

	t.Run("explicit path must exist", func(t *testing.T) {
		if _, err := Load(missing); err == nil {
			t.Fatal("expected an error for a missing explicit config path")
		}
	})

	t.Run("AMA_CONFIG path must exist", func(t *testing.T) {
		t.Setenv("AMA_CONFIG", missing)
		if _, err := Load(""); err == nil {
			t.Fatal("expected an error for a missing AMA_CONFIG path")
		}
	})

	t.Run("default path may be absent", func(t *testing.T) {
		t.Chdir(t.TempDir())
		got, err := Load("")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !reflect.DeepEqual(got, Default()) {
			t.Errorf("expected defaults, got %+v", got)
		}
	})
}

func TestValidate(t *testing.T) {
	// valid returns a config that passes Validate, for mutation per case.
	valid := func() *Config {
		c := Default()
		c.TMDB.APIKey = "tmdb"
		c.Radarr.APIKey = "radarr"
		return c
	}

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr []string
	}{
		{
			name:   "valid config",
			mutate: func(*Config) {},
		},
		{
			name:   "makemkv key may be empty",
			mutate: func(c *Config) { c.MakeMKV.Key = "" },
		},
		{
			name:    "missing tmdb api key",
			mutate:  func(c *Config) { c.TMDB.APIKey = "" },
			wantErr: []string{"tmdb.api_key"},
		},
		{
			name:    "radarr enabled without credentials",
			mutate:  func(c *Config) { c.Radarr.URL, c.Radarr.APIKey = "", "" },
			wantErr: []string{"radarr.url", "radarr.api_key"},
		},
		{
			name: "sonarr disabled needs nothing",
			mutate: func(c *Config) {
				c.Sonarr.Enabled = false
				c.Sonarr.URL, c.Sonarr.APIKey = "", ""
			},
		},
		{
			name:    "sonarr enabled without api key",
			mutate:  func(c *Config) { c.Sonarr.Enabled = true },
			wantErr: []string{"sonarr.api_key"},
		},
		{
			name:    "port too low",
			mutate:  func(c *Config) { c.Web.Port = 0 },
			wantErr: []string{"web.port"},
		},
		{
			name:    "port too high",
			mutate:  func(c *Config) { c.Web.Port = 70000 },
			wantErr: []string{"web.port"},
		},
		{
			name:    "poll interval below one",
			mutate:  func(c *Config) { c.Disc.PollInterval = 0 },
			wantErr: []string{"disc.poll_interval"},
		},
		{
			name:    "forced ratio at zero",
			mutate:  func(c *Config) { c.Subtitle.ForcedRatioThreshold = 0 },
			wantErr: []string{"subtitle.forced_ratio_threshold"},
		},
		{
			name:    "forced ratio at one",
			mutate:  func(c *Config) { c.Subtitle.ForcedRatioThreshold = 1 },
			wantErr: []string{"subtitle.forced_ratio_threshold"},
		},
		{
			name:    "negative min track duration",
			mutate:  func(c *Config) { c.MakeMKV.MinTrackDuration = -1 },
			wantErr: []string{"makemkv.min_track_duration"},
		},
		{
			name:    "auto confirm threshold out of range",
			mutate:  func(c *Config) { c.TMDB.AutoConfirmThreshold = floatPtr(1.5) },
			wantErr: []string{"tmdb.auto_confirm_threshold"},
		},
		{
			name:    "empty output paths",
			mutate:  func(c *Config) { c.Output = Output{} },
			wantErr: []string{"output.movies", "output.music", "output.temp"},
		},
		{
			name: "errors aggregate",
			mutate: func(c *Config) {
				c.TMDB.APIKey = ""
				c.Web.Port = -1
				c.Disc.PollInterval = 0
			},
			wantErr: []string{"tmdb.api_key", "web.port", "disc.poll_interval"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := valid()
			tt.mutate(c)

			err := c.Validate()
			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected errors %v, got nil", tt.wantErr)
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

func TestStringRedactsSecrets(t *testing.T) {
	c := Default()
	c.MakeMKV.Key = "mk-secret"
	c.TMDB.APIKey = "tmdb-secret"
	c.Radarr.APIKey = "radarr-secret"
	c.Sonarr.APIKey = "sonarr-secret"

	out := c.String()
	for _, secret := range []string{"mk-secret", "tmdb-secret", "radarr-secret", "sonarr-secret"} {
		if strings.Contains(out, secret) {
			t.Errorf("String() leaked %q:\n%s", secret, out)
		}
	}
	// Non-secret values are still visible, so the output stays useful.
	if !strings.Contains(out, "/dev/sr0") {
		t.Errorf("String() dropped non-secret values:\n%s", out)
	}
	// String must not mutate the receiver.
	if c.TMDB.APIKey != "tmdb-secret" {
		t.Errorf("String() mutated the config: tmdb.api_key = %q", c.TMDB.APIKey)
	}
}

// clearAMAEnv unsets every AMA_ variable for the duration of the test, so the
// developer's own environment cannot influence the result.
func clearAMAEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(key, "AMA_") {
			t.Setenv(key, "")
			os.Unsetenv(key)
		}
	}
}
