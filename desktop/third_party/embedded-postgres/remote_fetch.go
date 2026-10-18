package embeddedpostgres

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// maxArchiveBytes bounds a downloaded distribution. A real PostgreSQL
	// binary archive is tens of megabytes; the cap exists so a hostile or
	// misconfigured mirror cannot exhaust memory before verification.
	maxArchiveBytes = 512 << 20
	// maxChecksumBytes bounds the .sha256 sidecar. It is a single line.
	maxChecksumBytes = 4 << 10
	// downloadTimeout bounds the whole transfer. Without a timeout a stalled
	// mirror blocks application startup forever.
	downloadTimeout = 10 * time.Minute
)

// RemoteFetchStrategy provides a strategy to fetch a Postgres binary so that it is available for use.
type RemoteFetchStrategy func() error

//nolint:funlen
func defaultRemoteFetchStrategy(remoteFetchHost string, versionStrategy VersionStrategy, cacheLocator CacheLocator) RemoteFetchStrategy {
	return func() error {
		operatingSystem, architecture, version := versionStrategy()

		jarDownloadURL := fmt.Sprintf("%s/io/zonky/test/postgres/embedded-postgres-binaries-%s-%s/%s/embedded-postgres-binaries-%s-%s-%s.jar",
			remoteFetchHost,
			operatingSystem,
			architecture,
			version,
			operatingSystem,
			architecture,
			version)

		client := &http.Client{Timeout: downloadTimeout}

		jarDownloadResponse, err := client.Get(jarDownloadURL)
		if err != nil {
			return fmt.Errorf("unable to connect to %s", remoteFetchHost)
		}
		defer closeBody(jarDownloadResponse)

		if jarDownloadResponse.StatusCode != http.StatusOK {
			return fmt.Errorf("no version found matching %s", version)
		}

		jarBodyBytes, err := readCapped(jarDownloadResponse.Body, maxArchiveBytes)
		if err != nil {
			return errorFetchingPostgres(err)
		}

		// Verification is mandatory, not best effort. A mirror that omits the
		// sidecar, returns a 404, or truncates it must fail the install: the
		// archive is extracted and executed, so "no checksum" cannot mean "no
		// check". The old code silently skipped verification in exactly those
		// cases, which is what turned a compromised mirror into code execution.
		shaDownloadURL := fmt.Sprintf("%s.sha256", jarDownloadURL)
		shaDownloadResponse, err := client.Get(shaDownloadURL)
		if err != nil {
			return fmt.Errorf("checksum for %s is required but could not be downloaded: %w", jarDownloadURL, err)
		}
		defer closeBody(shaDownloadResponse)

		if shaDownloadResponse.StatusCode != http.StatusOK {
			return fmt.Errorf("checksum for %s is required but the server returned %s",
				jarDownloadURL, shaDownloadResponse.Status)
		}

		shaBodyBytes, err := readCapped(shaDownloadResponse.Body, maxChecksumBytes)
		if err != nil {
			return fmt.Errorf("read checksum for %s: %w", jarDownloadURL, err)
		}
		expected, err := parseChecksum(shaBodyBytes)
		if err != nil {
			return fmt.Errorf("checksum for %s is invalid: %w", jarDownloadURL, err)
		}
		actual := sha256.Sum256(jarBodyBytes)
		if subtle.ConstantTimeCompare(expected, []byte(hex.EncodeToString(actual[:]))) != 1 {
			return fmt.Errorf("downloaded checksum does not match for %s", jarDownloadURL)
		}

		return decompressResponse(jarBodyBytes, jarDownloadResponse.ContentLength, cacheLocator, jarDownloadURL)
	}
}

// readCapped reads at most limit bytes and fails when the source is larger, so
// a hostile response cannot exhaust memory before it is verified.
func readCapped(r io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("response exceeded the %d byte limit", limit)
	}
	return body, nil
}

// parseChecksum accepts the two formats checksum files are published in:
// the bare digest, and "<digest>  <filename>". Anything that is not a 64-hex
// SHA-256 digest is rejected rather than compared loosely.
func parseChecksum(body []byte) ([]byte, error) {
	fields := strings.Fields(string(body))
	if len(fields) == 0 {
		return nil, fmt.Errorf("checksum file is empty")
	}
	digest := strings.ToLower(fields[0])
	if len(digest) != sha256.Size*2 {
		return nil, fmt.Errorf("checksum is not a sha256 digest")
	}
	decoded, err := hex.DecodeString(digest)
	if err != nil {
		return nil, fmt.Errorf("checksum is not hexadecimal")
	}
	return decoded, nil
}

// closeBody closes a response body without terminating the process. A close
// error on a fully read body carries no actionable information, and the previous
// log.Fatal here turned a normal cleanup failure into an application exit.
func closeBody(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_ = resp.Body.Close()
}

func decompressResponse(bodyBytes []byte, contentLength int64, cacheLocator CacheLocator, downloadURL string) error {
	size := contentLength
	// if the content length is not set (i.e. chunked encoding),
	// we need to use the length of the bodyBytes otherwise
	// the unzip operation will fail
	if contentLength < 0 {
		size = int64(len(bodyBytes))
	}
	zipReader, err := zip.NewReader(bytes.NewReader(bodyBytes), size)
	if err != nil {
		return errorFetchingPostgres(err)
	}

	cacheLocation, _ := cacheLocator()

	if err := os.MkdirAll(filepath.Dir(cacheLocation), 0755); err != nil {
		return errorExtractingPostgres(err)
	}

	for _, file := range zipReader.File {
		if !file.FileHeader.FileInfo().IsDir() && strings.HasSuffix(file.FileHeader.Name, ".txz") {
			if err := decompressSingleFile(file, cacheLocation); err != nil {
				return err
			}

			// we have successfully found the file, return early
			return nil
		}
	}

	return fmt.Errorf("error fetching postgres: cannot find binary in archive retrieved from %s", downloadURL)
}

func decompressSingleFile(file *zip.File, cacheLocation string) error {
	renamed := false

	archiveReader, err := file.Open()
	if err != nil {
		return errorExtractingPostgres(err)
	}

	archiveBytes, err := io.ReadAll(archiveReader)
	if err != nil {
		return errorExtractingPostgres(err)
	}

	// if multiple processes attempt to extract
	// to prevent file corruption when multiple processes attempt to extract at the same time
	// first to a cache location, and then move the file into place.
	tmp, err := os.CreateTemp(filepath.Dir(cacheLocation), "temp_")
	if err != nil {
		return errorExtractingPostgres(err)
	}
	defer func() {
		// if anything failed before the rename then the temporary file should be cleaned up.
		// if the rename was successful then there is no temporary file to remove.
		if !renamed {
			if err := os.Remove(tmp.Name()); err != nil {
				panic(err)
			}
		}
	}()

	if _, err := tmp.Write(archiveBytes); err != nil {
		return errorExtractingPostgres(err)
	}

	// Windows cannot rename a file if is it still open.
	// The file needs to be manually closed to allow the rename to happen
	if err := tmp.Close(); err != nil {
		return errorExtractingPostgres(err)
	}

	if err := renameOrIgnore(tmp.Name(), cacheLocation); err != nil {
		return errorExtractingPostgres(err)
	}
	renamed = true

	return nil
}

func errorExtractingPostgres(err error) error {
	return fmt.Errorf("unable to extract postgres archive: %s", err)
}

func errorFetchingPostgres(err error) error {
	return fmt.Errorf("error fetching postgres: %s", err)
}
