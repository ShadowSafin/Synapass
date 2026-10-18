package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"testing/fstest"
)

const sampleSQL = "-- a comment\nCREATE TABLE t (\n    id UUID PRIMARY KEY\n);\n"

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func loadOne(t *testing.T, content string) Migration {
	t.Helper()
	fsys := fstest.MapFS{
		"migrations/0001_sample.sql": &fstest.MapFile{Data: []byte(content)},
	}
	migrations, err := LoadMigrations(fsys, "migrations")
	if err != nil {
		t.Fatalf("LoadMigrations: %v", err)
	}
	if len(migrations) != 1 {
		t.Fatalf("want one migration, got %d", len(migrations))
	}
	return migrations[0]
}

func TestLoadMigrationsChecksumIgnoresLineEndings(t *testing.T) {
	// The same commit is checked out with LF on Linux and CRLF on a Windows
	// host with autocrlf. The checksum decides whether a migration was edited
	// after it was applied, which must not depend on the checkout platform.
	lf := loadOne(t, sampleSQL)
	crlf := loadOne(t, "-- a comment\r\nCREATE TABLE t (\r\n    id UUID PRIMARY KEY\r\n);\r\n")

	if lf.Checksum != crlf.Checksum {
		t.Fatalf("line endings changed the checksum: lf=%s crlf=%s", lf.Checksum, crlf.Checksum)
	}
	if want := sha256Hex(sampleSQL); lf.Checksum != want {
		t.Fatalf("checksum is not the LF hash: got %s want %s", lf.Checksum, want)
	}
}

func TestChecksumMatchesLegacyLineEndingRecords(t *testing.T) {
	// A migration applied before normalization was recorded with the bytes as
	// checked out. Both legacy forms must still match, or upgrading refuses to
	// start on the very installations the normalization is meant to help.
	lfHash := sha256Hex(sampleSQL)
	crlfHash := sha256Hex("-- a comment\r\nCREATE TABLE t (\r\n    id UUID PRIMARY KEY\r\n);\r\n")

	for name, recorded := range map[string]string{
		"lf":   lfHash,
		"crlf": crlfHash,
	} {
		if !checksumMatches(recorded, sampleSQL) {
			t.Errorf("recorded %s checksum should match a migration now read with LF", name)
		}
		if !checksumMatches(recorded, "-- a comment\r\nCREATE TABLE t (\r\n    id UUID PRIMARY KEY\r\n);\r\n") {
			t.Errorf("recorded %s checksum should match a migration read with CRLF", name)
		}
	}
}

func TestChecksumMatchesRejectsARealEdit(t *testing.T) {
	edited := "-- a comment\nCREATE TABLE t (\n    id UUID PRIMARY KEY,\n    note TEXT\n);\n"
	if checksumMatches(sha256Hex(sampleSQL), edited) {
		t.Fatal("a real content edit must not match the recorded checksum")
	}
}
