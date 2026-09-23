package main

import (
	"errors"
	"io"
	"os"
	"testing"
)

// discardFile is an *os.File whose writes go nowhere, for flag output.
func discardFile(t *testing.T) *os.File {
	t.Helper()
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Skipf("no %s available: %v", os.DevNull, err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func TestParseFlags(t *testing.T) {
	tests := []struct {
		name           string
		args           []string
		wantConfigPath string
		wantVersion    bool
		wantErr        bool
	}{
		{name: "no flags", args: nil},
		{name: "config path", args: []string{"-config", "/etc/ama.yaml"}, wantConfigPath: "/etc/ama.yaml"},
		{name: "version", args: []string{"-version"}, wantVersion: true},
		{name: "both", args: []string{"-config", "/etc/ama.yaml", "-version"}, wantConfigPath: "/etc/ama.yaml", wantVersion: true},
		{name: "unknown flag is an error", args: []string{"-nope"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fset := newFlagSet(discardFile(t))
			configPath, showVersion, err := parseFlags(fset, tt.args)
			if tt.wantErr {
				if err == nil || errors.Is(err, errFlagHelp) {
					t.Fatalf("parseFlags(%v): want a real error, got %v", tt.args, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseFlags(%v): %v", tt.args, err)
			}
			if configPath != tt.wantConfigPath {
				t.Errorf("configPath = %q, want %q", configPath, tt.wantConfigPath)
			}
			if showVersion != tt.wantVersion {
				t.Errorf("showVersion = %v, want %v", showVersion, tt.wantVersion)
			}
		})
	}
}

func TestParseFlagsHelp(t *testing.T) {
	fset := newFlagSet(discardFile(t))
	_, _, err := parseFlags(fset, []string{"-h"})
	if !errors.Is(err, errFlagHelp) {
		t.Errorf("parseFlags(-h) error = %v, want errFlagHelp", err)
	}
}

func TestNewLogger(t *testing.T) {
	tests := []struct {
		envLevel string
	}{
		{envLevel: ""},
		{envLevel: "debug"},
		{envLevel: "DEBUG"},
		{envLevel: "warn"},
		{envLevel: "error"},
		{envLevel: "not-a-real-level"},
	}
	for _, tt := range tests {
		t.Run("level="+tt.envLevel, func(t *testing.T) {
			t.Setenv("AMA_LOG_LEVEL", tt.envLevel)
			f, err := os.CreateTemp(t.TempDir(), "log")
			if err != nil {
				t.Fatalf("creating temp file: %v", err)
			}
			defer func() { _ = f.Close() }()

			logger := newLogger(f)
			if logger == nil {
				t.Fatal("newLogger returned nil")
			}
			// Error is always enabled regardless of the configured minimum
			// level, so this proves the logger writes at all without
			// depending on what level ended up selected.
			logger.Error("hello")
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				t.Fatalf("seeking: %v", err)
			}
			data, err := io.ReadAll(f)
			if err != nil {
				t.Fatalf("reading log output: %v", err)
			}
			if len(data) == 0 {
				t.Error("logger wrote nothing")
			}
		})
	}
}
