package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/gastownhall/gascity/internal/fsys"
)

// MinFileArchiveAfter is the smallest beads.file_archive_after config accepts.
// The archive cutoff moves in quarter-retention steps and every step rewrites
// the archive, so a tiny retention would turn the archive into a second hot
// file.
const MinFileArchiveAfter = time.Hour

// FileArchiveAfterDuration resolves beads.file_archive_after: how long a closed
// bead must go without updates before a file-backed store moves it from the
// hot beads.json (rewritten on every mutation) to beads.archive.json. set is
// false when the key is empty (the default: archive nothing new, keep any
// existing archive as it is); "0" and "off" are set with a zero duration
// (fold an existing archive back into beads.json).
func (b BeadsConfig) FileArchiveAfterDuration() (after time.Duration, set bool, err error) {
	raw := strings.TrimSpace(b.FileArchiveAfter)
	switch strings.ToLower(raw) {
	case "":
		return 0, false, nil
	case "0", "off":
		return 0, true, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, false, fmt.Errorf("beads.file_archive_after: %w", err)
	}
	if d < MinFileArchiveAfter {
		return 0, false, fmt.Errorf("beads.file_archive_after: %s is below the minimum %s (use \"off\" to disable)", d, MinFileArchiveAfter)
	}
	return d, true, nil
}

// ReadRootFileArchiveAfter reads only [beads] file_archive_after from the
// root city.toml at path, ignoring includes and every other setting. It is
// for callers that still open a file store when the full config does not load
// (gc doctor --fix, legacy layouts): they must not drop the archive setting
// along with the rest — a mistyped "off" before a rollback would go unnoticed.
// A file that cannot be read or parsed as TOML has no readable setting and
// yields the zero config; a value that is present but not a string is an
// error.
func ReadRootFileArchiveAfter(fs fsys.FS, path string) (BeadsConfig, error) {
	data, err := fs.ReadFile(path)
	if err != nil {
		return BeadsConfig{}, nil
	}
	var raw struct {
		Beads struct {
			FileArchiveAfter any `toml:"file_archive_after"`
		} `toml:"beads"`
	}
	if _, err := toml.Decode(string(data), &raw); err != nil {
		return BeadsConfig{}, nil
	}
	switch v := raw.Beads.FileArchiveAfter.(type) {
	case nil:
		return BeadsConfig{}, nil
	case string:
		return BeadsConfig{FileArchiveAfter: v}, nil
	default:
		return BeadsConfig{}, fmt.Errorf("beads.file_archive_after: %v is a %T, want a duration string such as \"24h\" or \"off\"", v, v)
	}
}

func validateFileArchiveAfter(b BeadsConfig) error {
	_, _, err := b.FileArchiveAfterDuration()
	return err
}
