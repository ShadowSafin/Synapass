package embeddedpostgres

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSafeArchiveRelPathRejectsEscapes(t *testing.T) {
	bad := []string{
		"",
		"../evil",
		"../../evil",
		"a/../../evil",
		"/etc/passwd",
		"\\etc\\passwd",
		"C:/Windows/system32/evil.dll",
		"C:\\Windows\\evil.dll",
		"//server/share/evil",
		"./../../evil",
		"a\x00b",
	}
	for _, name := range bad {
		if _, err := safeArchiveRelPath(name); err == nil {
			t.Errorf("safeArchiveRelPath(%q) accepted a path outside the extraction root", name)
		}
	}
}

func TestSafeArchiveRelPathAcceptsNormalMembers(t *testing.T) {
	cases := map[string]string{
		"bin/postgres":       filepath.Join("bin", "postgres"),
		"./bin/initdb":       filepath.Join("bin", "initdb"),
		"share/timezone/UTC": filepath.Join("share", "timezone", "UTC"),
		"lib\\libpq.so":      filepath.Join("lib", "libpq.so"),
		"a/../b":             "b",
		"nested/dir/":        filepath.Join("nested", "dir"),
	}
	for name, want := range cases {
		got, err := safeArchiveRelPath(name)
		if err != nil {
			t.Errorf("safeArchiveRelPath(%q): %v", name, err)
			continue
		}
		if got != want {
			t.Errorf("safeArchiveRelPath(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestSafeLinkTargetConfinesWithinRoot(t *testing.T) {
	root := filepath.Join("extract", "postgres")

	// A relative link that stays inside the tree is what a distribution uses.
	linkPath := filepath.Join(root, "lib", "libpq.so.5")
	if _, err := safeLinkTarget(root, linkPath, "libpq.so.5.15"); err != nil {
		t.Fatalf("a contained relative link must be allowed: %v", err)
	}

	bad := []string{
		"/etc/passwd",
		"../../../../etc/passwd",
		"../../../Windows/System32/x.dll",
		"../../",
		"",
	}
	for _, target := range bad {
		if _, err := safeLinkTarget(root, linkPath, target); err == nil {
			t.Errorf("link target %q escaped the extraction root", target)
		}
	}
}

func TestArchiveModeDropsSetuidBits(t *testing.T) {
	// 0o4755 is setuid root; only the permission bits may survive.
	if got := archiveMode(0o4755, 0o644); got != 0o755 {
		t.Errorf("archiveMode(04755) = %o, want 0755", got)
	}
	if got := archiveMode(0, 0o644); got != 0o644 {
		t.Errorf("a zero mode must fall back, got %o", got)
	}
}

func TestParseChecksumAcceptsPublishedFormats(t *testing.T) {
	const digest = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	for _, body := range []string{
		digest,
		digest + "\n",
		digest + "  embedded-postgres-binaries-linux-amd64.jar",
		strings.ToUpper(digest) + "\n",
	} {
		got, err := parseChecksum([]byte(body))
		if err != nil {
			t.Errorf("parseChecksum(%q): %v", body, err)
			continue
		}
		if len(got) != 32 {
			t.Errorf("parseChecksum(%q) returned %d bytes, want 32", body, len(got))
		}
	}

	bad := []string{"", "not-a-digest", digest[:63], digest + "00", "zz" + digest[2:]}
	for _, body := range bad {
		if _, err := parseChecksum([]byte(body)); err == nil {
			t.Errorf("parseChecksum(%q) accepted an invalid digest", body)
		}
	}
}

func TestReadCappedRejectsOversizedResponses(t *testing.T) {
	if _, err := readCapped(strings.NewReader("12345"), 4); err == nil {
		t.Fatal("readCapped must fail when the body exceeds the limit")
	}
	got, err := readCapped(strings.NewReader("1234"), 4)
	if err != nil || string(got) != "1234" {
		t.Fatalf("readCapped = %q, %v", got, err)
	}
}
