package binkit

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/ulikunitz/xz"
)

// extractBinary pulls exactly one entry — the one whose path inside the archive equals
// innerPath — out of archivePath and writes it to destDir/destName with mode 0755.
//
// Only the single expected entry is ever written, and destName is chosen by binkit
// rather than taken from the archive, so a hostile archive has no path to traverse.
// [os.Root] confines the write as a second line of defence. Extraction always runs
// after the archive's digest has been verified, so the bytes here are already the
// bytes that were pinned.
func extractBinary(archivePath, assetName, innerPath, destDir, destName string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open archive %s: %w", archivePath, err)
	}
	defer f.Close()

	name := strings.ToLower(assetName)
	switch {
	case strings.HasSuffix(name, ".tar.xz"), strings.HasSuffix(name, ".txz"):
		xr, err := xz.NewReader(f)
		if err != nil {
			return fmt.Errorf("open xz stream in %s: %w", assetName, err)
		}
		return extractFromTar(xr, innerPath, destDir, destName)

	case strings.HasSuffix(name, ".tar.gz"), strings.HasSuffix(name, ".tgz"):
		gr, err := gzip.NewReader(f)
		if err != nil {
			return fmt.Errorf("open gzip stream in %s: %w", assetName, err)
		}
		defer gr.Close()
		return extractFromTar(gr, innerPath, destDir, destName)

	case strings.HasSuffix(name, ".tar"):
		return extractFromTar(f, innerPath, destDir, destName)

	case strings.HasSuffix(name, ".zip"):
		return extractFromZip(archivePath, innerPath, destDir, destName)

	default:
		return fmt.Errorf("%w: %s", ErrUnsupportedArchive, assetName)
	}
}

func extractFromTar(src io.Reader, innerPath, destDir, destName string) error {
	tr := tar.NewReader(src)
	want := path.Clean(innerPath)

	var seen []string
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar entry: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		seen = append(seen, hdr.Name)
		if path.Clean(hdr.Name) == want {
			return writeBinary(destDir, destName, tr)
		}
	}
	return notFound(innerPath, seen)
}

func extractFromZip(archivePath, innerPath, destDir, destName string) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("open zip %s: %w", archivePath, err)
	}
	defer zr.Close()

	want := path.Clean(innerPath)

	var seen []string
	for _, entry := range zr.File {
		if entry.FileInfo().IsDir() {
			continue
		}
		seen = append(seen, entry.Name)
		if path.Clean(entry.Name) != want {
			continue
		}
		rc, err := entry.Open()
		if err != nil {
			return fmt.Errorf("open %s inside zip: %w", entry.Name, err)
		}
		defer rc.Close()
		return writeBinary(destDir, destName, rc)
	}
	return notFound(innerPath, seen)
}

// writeBinary creates destName inside destDir with the executable bit set.
func writeBinary(destDir, destName string, src io.Reader) error {
	root, err := os.OpenRoot(destDir)
	if err != nil {
		return fmt.Errorf("open destination %s: %w", destDir, err)
	}
	defer root.Close()

	f, err := root.OpenFile(destName, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o755)
	if err != nil {
		return fmt.Errorf("create %s: %w", destName, err)
	}
	if _, err := io.Copy(f, src); err != nil {
		f.Close()
		return fmt.Errorf("write %s: %w", destName, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", destName, err)
	}

	// The create mode above is masked by umask, which commonly strips the exec bit.
	// Set it explicitly — an unrunnable binary is the whole failure mode here.
	full := filepath.Join(destDir, destName)
	if err := os.Chmod(full, 0o755); err != nil {
		return fmt.Errorf("make %s executable: %w", full, err)
	}
	return nil
}

// notFound reports a missing entry along with what the archive actually held, so an
// upstream change to the archive layout is diagnosable from the error alone.
func notFound(want string, seen []string) error {
	const maxListed = 12
	listed := seen
	suffix := ""
	if len(listed) > maxListed {
		listed, suffix = listed[:maxListed], fmt.Sprintf(" (+%d more)", len(seen)-maxListed)
	}
	if len(listed) == 0 {
		return fmt.Errorf("%w: %q; archive contained no regular files", ErrBinaryNotInArchive, want)
	}
	return fmt.Errorf("%w: %q; archive contains: %s%s",
		ErrBinaryNotInArchive, want, strings.Join(listed, ", "), suffix)
}
