package config

import (
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestFileArchiveAfterDuration(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantSet bool
		wantErr string
	}{
		{in: "", want: 0, wantSet: false},
		{in: "0", want: 0, wantSet: true},
		{in: "off", want: 0, wantSet: true},
		{in: " OFF ", want: 0, wantSet: true},
		{in: "24h", want: 24 * time.Hour, wantSet: true},
		{in: "1h", want: time.Hour, wantSet: true},
		{in: "59m", wantErr: "below the minimum"},
		{in: "-2h", wantErr: "below the minimum"},
		{in: "daily", wantErr: "beads.file_archive_after"},
	}
	for _, tc := range cases {
		got, set, err := BeadsConfig{FileArchiveAfter: tc.in}.FileArchiveAfterDuration()
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("FileArchiveAfterDuration(%q) error = %v, want containing %q", tc.in, err, tc.wantErr)
			}
			continue
		}
		if err != nil || got != tc.want || set != tc.wantSet {
			t.Errorf("FileArchiveAfterDuration(%q) = %v, %v, %v; want %v, %v", tc.in, got, set, err, tc.want, tc.wantSet)
		}
	}
}

// A typo must fail config load rather than silently leave archiving off.
func TestParseRejectsInvalidFileArchiveAfter(t *testing.T) {
	_, err := Parse([]byte("[workspace]\nname = \"t\"\n\n[beads]\nfile_archive_after = \"10m\"\n"))
	if err == nil || !strings.Contains(err.Error(), "file_archive_after") {
		t.Fatalf("Parse error = %v, want file_archive_after rejection", err)
	}
}

func TestReadRootFileArchiveAfter(t *testing.T) {
	fs := fsys.NewFake()
	for _, tc := range []struct {
		name, toml, want, wantErr string
	}{
		{name: "string", toml: "[beads]\nfile_archive_after = \"24h\"\n", want: "24h"},
		{name: "absent", toml: "[workspace]\nname = \"t\"\n"},
		{name: "not toml", toml: "[beads\n"},
		{name: "integer", toml: "[beads]\nfile_archive_after = 24\n", wantErr: "file_archive_after"},
	} {
		fs.Files["/city/city.toml"] = []byte(tc.toml)
		got, err := ReadRootFileArchiveAfter(fs, "/city/city.toml")
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%s: error = %v, want containing %q", tc.name, err, tc.wantErr)
			}
			continue
		}
		if err != nil || got.FileArchiveAfter != tc.want {
			t.Errorf("%s: = %q, %v; want %q", tc.name, got.FileArchiveAfter, err, tc.want)
		}
	}
}

func TestLoadWithIncludesPreservesFileArchiveAfterAcrossBeadsFragment(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["fragment.toml"]

[workspace]
name = "test"

[beads]
file_archive_after = "24h"
`)
	fs.Files["/city/fragment.toml"] = []byte(`
[beads]
bd_compatibility = "bd-1.0.5"
`)
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if cfg.Beads.FileArchiveAfter != "24h" {
		t.Fatalf("FileArchiveAfter = %q, want root 24h to survive a [beads] fragment", cfg.Beads.FileArchiveAfter)
	}

	fs.Files["/city/fragment.toml"] = []byte(`
[beads]
file_archive_after = "off"
`)
	cfg, _, err = LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if cfg.Beads.FileArchiveAfter != "off" {
		t.Fatalf("FileArchiveAfter = %q, want the fragment's off to win", cfg.Beads.FileArchiveAfter)
	}
}
