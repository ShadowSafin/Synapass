package embeddedpostgres

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/xi2/xz"
)

func defaultTarReader(xzReader *xz.Reader) (func() (*tar.Header, error), func() io.Reader) {
	tarReader := tar.NewReader(xzReader)

	return func() (*tar.Header, error) {
			return tarReader.Next()
		}, func() io.Reader {
			return tarReader
		}
}

// safeArchiveRelPath turns an archive member name into a relative path that is
// guaranteed to stay inside the extraction root.
//
// The archive is downloaded over the network, so a member name is untrusted
// input: "../x", "/etc/cron.d/x", "C:/Windows/x" and "a/../../x" all have to be
// refused rather than cleaned into something plausible. Both separators are
// normalised first because a tar written on Windows can carry backslashes, and a
// name containing a NUL is rejected outright.
func safeArchiveRelPath(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("empty archive member name")
	}
	if strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("archive member name contains a NUL byte")
	}

	normalized := strings.ReplaceAll(name, "\\", "/")
	if strings.HasPrefix(normalized, "/") {
		return "", fmt.Errorf("archive member is an absolute path")
	}
	// A drive letter or UNC prefix is absolute on Windows even though
	// filepath.IsAbs on a non-Windows host would disagree.
	if len(normalized) >= 2 && normalized[1] == ':' {
		return "", fmt.Errorf("archive member carries a drive letter")
	}
	if strings.HasPrefix(normalized, "//") {
		return "", fmt.Errorf("archive member is a UNC path")
	}

	clean := path.Clean(normalized)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("archive member escapes the extraction root")
	}
	return filepath.FromSlash(clean), nil
}

// withinRoot reports whether target is root or a descendant of it. Both paths
// must already be absolute or both relative to the same directory.
func withinRoot(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// safeLinkTarget validates a symbolic link target and returns the resolved
// destination, which must remain inside root.
//
// Absolute links and links that climb out of the tree are refused: the extracted
// tree is later executed, so a link such as "../../startup/x" is a write
// primitive, not a convenience.
func safeLinkTarget(root, linkPath, target string) (string, error) {
	if target == "" {
		return "", fmt.Errorf("symbolic link has an empty target")
	}
	normalized := strings.ReplaceAll(target, "\\", "/")
	if strings.HasPrefix(normalized, "/") || (len(normalized) >= 2 && normalized[1] == ':') {
		return "", fmt.Errorf("symbolic link target is absolute")
	}
	resolved := filepath.Join(filepath.Dir(linkPath), filepath.FromSlash(normalized))
	if !withinRoot(root, resolved) {
		return "", fmt.Errorf("symbolic link target escapes the extraction root")
	}
	return resolved, nil
}

// archiveMode masks an archive-supplied mode to permission bits, dropping
// setuid/setgid/sticky flags. The extracted binaries are executed, so a mode
// field must not be able to install a setuid helper.
func archiveMode(mode int64, fallback os.FileMode) os.FileMode {
	masked := os.FileMode(mode) & os.ModePerm
	if masked == 0 {
		return fallback
	}
	return masked
}

func decompressTarXz(tarReader func(*xz.Reader) (func() (*tar.Header, error), func() io.Reader), path, extractPath string) error {
	extractDirectory := filepath.Dir(extractPath)

	if err := os.MkdirAll(extractDirectory, 0o755); err != nil {
		return errorUnableToExtract(path, extractPath, err)
	}

	tempExtractPath, err := os.MkdirTemp(extractDirectory, "temp_")
	if err != nil {
		return errorUnableToExtract(path, extractPath, err)
	}
	defer func() {
		// Best effort: a leftover temporary directory is not a reason to crash
		// the host application, which is what the previous panic did.
		_ = os.RemoveAll(tempExtractPath)
	}()

	tarFile, err := os.Open(path)
	if err != nil {
		return errorUnableToExtract(path, extractPath, err)
	}
	defer func() {
		_ = tarFile.Close()
	}()

	xzReader, err := xz.NewReader(tarFile, 0)
	if err != nil {
		return errorUnableToExtract(path, extractPath, err)
	}

	readNext, reader := tarReader(xzReader)

	for {
		header, err := readNext()

		if err == io.EOF {
			break
		}

		if err != nil {
			return errorExtractingPostgres(err)
		}

		rel, err := safeArchiveRelPath(header.Name)
		if err != nil {
			return errorExtractingPostgres(fmt.Errorf("refusing entry %q: %w", header.Name, err))
		}

		targetPath := filepath.Join(tempExtractPath, rel)
		finalPath := filepath.Join(extractPath, rel)

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(targetPath, archiveMode(header.Mode, 0o755)); err != nil {
				return errorExtractingPostgres(err)
			}
			if err := os.MkdirAll(finalPath, archiveMode(header.Mode, 0o755)); err != nil {
				return errorExtractingPostgres(err)
			}
			continue

		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
				return errorExtractingPostgres(err)
			}
			if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
				return errorExtractingPostgres(err)
			}
			outFile, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, archiveMode(header.Mode, 0o644))
			if err != nil {
				return errorExtractingPostgres(err)
			}
			if _, err := io.Copy(outFile, reader()); err != nil {
				_ = outFile.Close()
				return errorExtractingPostgres(err)
			}
			if err := outFile.Close(); err != nil {
				return errorExtractingPostgres(err)
			}

		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
				return errorExtractingPostgres(err)
			}
			if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
				return errorExtractingPostgres(err)
			}
			if _, err := safeLinkTarget(extractPath, finalPath, header.Linkname); err != nil {
				return errorExtractingPostgres(fmt.Errorf("refusing symlink %q: %w", header.Name, err))
			}
			if err := os.RemoveAll(targetPath); err != nil {
				return errorExtractingPostgres(err)
			}
			if err := os.Symlink(header.Linkname, targetPath); err != nil {
				return errorExtractingPostgres(err)
			}

		default:
			// Devices, FIFOs and hard links have no business in a PostgreSQL
			// binary distribution, and each is a way to reach outside the tree.
			return errorExtractingPostgres(fmt.Errorf("refusing unsupported archive entry type for %q", header.Name))
		}

		if err := renameOrIgnore(targetPath, finalPath); err != nil {
			return errorExtractingPostgres(err)
		}
	}

	return nil
}

func errorUnableToExtract(cacheLocation, binariesPath string, err error) error {
	return fmt.Errorf("unable to extract postgres archive %s to %s, if running parallel tests, configure RuntimePath to isolate testing directories, %w",
		cacheLocation,
		binariesPath,
		err,
	)
}
