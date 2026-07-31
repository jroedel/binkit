package binkit

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractBinarySupportedFormats(t *testing.T) {
	const inner = "tool-1.0.0-linux-amd64/tool"
	payload := []byte("#!/bin/sh\necho hello\n")

	tests := []struct {
		name  string
		asset string
		build func(*testing.T, string, []byte) []byte
	}{
		{"tar.xz", "tool.tar.xz", buildTarXZ},
		{"txz", "tool.txz", buildTarXZ},
		{"tar.gz", "tool.tar.gz", buildTarGZ},
		{"tgz", "tool.tgz", buildTarGZ},
		{"zip", "tool.zip", buildZip},
		{"uppercase extension", "TOOL.TAR.XZ", buildTarXZ},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			archiveDir := t.TempDir()
			archive := filepath.Join(archiveDir, "archive")
			if err := os.WriteFile(archive, tc.build(t, inner, payload), 0o600); err != nil {
				t.Fatalf("write archive: %v", err)
			}

			dest := t.TempDir()
			if err := extractBinary(archive, tc.asset, inner, dest, "tool"); err != nil {
				t.Fatalf("extractBinary: %v", err)
			}

			out := filepath.Join(dest, "tool")
			got, err := os.ReadFile(out)
			if err != nil {
				t.Fatalf("read extracted binary: %v", err)
			}
			if string(got) != string(payload) {
				t.Errorf("extracted %q, want %q", got, payload)
			}

			info, err := os.Stat(out)
			if err != nil {
				t.Fatalf("stat extracted binary: %v", err)
			}
			if info.Mode().Perm()&0o111 == 0 {
				t.Errorf("extracted binary is not executable: mode %v", info.Mode().Perm())
			}
		})
	}
}

// TestExtractBinaryWritesOnlyTheWantedEntry guards the extraction contract: whatever
// else an archive contains, only the single expected binary reaches the destination.
func TestExtractBinaryWritesOnlyTheWantedEntry(t *testing.T) {
	const inner = "pkg/tool"
	archiveDir := t.TempDir()
	archive := filepath.Join(archiveDir, "archive")
	if err := os.WriteFile(archive, buildTarXZ(t, inner, []byte("payload")), 0o600); err != nil {
		t.Fatalf("write archive: %v", err)
	}

	dest := t.TempDir()
	if err := extractBinary(archive, "a.tar.xz", inner, dest, "tool"); err != nil {
		t.Fatalf("extractBinary: %v", err)
	}

	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "tool" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("destination holds %v, want exactly [tool] — the archive's directory "+
			"entry must not be recreated", names)
	}
}

func TestExtractBinaryUnsupportedFormat(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "archive")
	if err := os.WriteFile(archive, []byte("not an archive"), 0o600); err != nil {
		t.Fatalf("write archive: %v", err)
	}

	err := extractBinary(archive, "tool.tar.bz2", "tool", t.TempDir(), "tool")
	if !errors.Is(err, ErrUnsupportedArchive) {
		t.Fatalf("extractBinary error = %v, want ErrUnsupportedArchive", err)
	}
	if !strings.Contains(err.Error(), "tool.tar.bz2") {
		t.Errorf("error %q does not name the offending asset", err)
	}
}

// TestExtractBinaryMissingEntryListsContents matters for diagnosis: when upstream
// changes its archive layout, the error alone should be enough to see what moved.
func TestExtractBinaryMissingEntryListsContents(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "archive")
	if err := os.WriteFile(archive, buildTarXZ(t, "actual/location/tool", []byte("x")), 0o600); err != nil {
		t.Fatalf("write archive: %v", err)
	}

	err := extractBinary(archive, "a.tar.xz", "expected/location/tool", t.TempDir(), "tool")
	if !errors.Is(err, ErrBinaryNotInArchive) {
		t.Fatalf("extractBinary error = %v, want ErrBinaryNotInArchive", err)
	}
	if !strings.Contains(err.Error(), "expected/location/tool") {
		t.Errorf("error %q does not state what was looked for", err)
	}
	if !strings.Contains(err.Error(), "actual/location/tool") {
		t.Errorf("error %q does not list what the archive actually contained", err)
	}
}

func TestExtractBinaryCorruptStream(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "archive")
	if err := os.WriteFile(archive, []byte("definitely not xz data"), 0o600); err != nil {
		t.Fatalf("write archive: %v", err)
	}

	if err := extractBinary(archive, "tool.tar.xz", "tool", t.TempDir(), "tool"); err == nil {
		t.Fatal("extractBinary on a corrupt stream succeeded, want an error")
	}
}

func TestExtractBinaryZipMissingEntry(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "archive")
	if err := os.WriteFile(archive, buildZip(t, "pkg/other", []byte("x")), 0o600); err != nil {
		t.Fatalf("write archive: %v", err)
	}

	err := extractBinary(archive, "tool.zip", "pkg/tool.exe", t.TempDir(), "tool.exe")
	if !errors.Is(err, ErrBinaryNotInArchive) {
		t.Fatalf("extractBinary error = %v, want ErrBinaryNotInArchive", err)
	}
	if !strings.Contains(err.Error(), "pkg/other") {
		t.Errorf("error %q does not list the archive's actual contents", err)
	}
}
