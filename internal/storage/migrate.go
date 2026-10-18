package storage

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/shadowsafin/synapass/internal/domain"
)

// Migrations are embedded so a binary is self-contained. That matters for the
// native install path: an operator should be able to copy one executable to a
// host and run `synapass migrate` without also shipping a directory of SQL
// files that could drift from the binary that reads them.
//
//go:embed migrations/postgres/*.sql
var postgresMigrations embed.FS

//go:embed migrations/clickhouse/*.sql
var clickhouseMigrations embed.FS

// Migration is a single versioned schema change.
type Migration struct {
	// Version is the numeric prefix of the filename, zero-padded so lexical and
	// numeric ordering agree.
	Version string
	// Name is the remainder of the filename.
	Name string
	// SQL is the statement text.
	SQL string
	// Checksum detects a migration file edited after it was applied, which would
	// otherwise leave environments silently divergent.
	Checksum string
}

// Filename returns the migration's display name.
func (m Migration) Filename() string {
	if m.Name == "" {
		return m.Version + ".sql"
	}
	return m.Version + "_" + m.Name + ".sql"
}

// LoadMigrations reads migrations from a filesystem directory.
//
// Files must be named <version>_<name>.sql or <version>.sql. They are returned in
// ascending version order, so applying them in slice order is correct.
func LoadMigrations(fsys fs.FS, dir string) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("read migrations directory %q: %w", dir, err)
	}

	out := make([]Migration, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		content, err := fs.ReadFile(fsys, filepath.ToSlash(filepath.Join(dir, entry.Name())))
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", entry.Name(), err)
		}
		base := strings.TrimSuffix(entry.Name(), ".sql")
		version, name, _ := strings.Cut(base, "_")

		out = append(out, Migration{
			Version:  version,
			Name:     name,
			SQL:      string(content),
			Checksum: migrationChecksum(content),
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// migrationChecksum hashes a migration with line endings normalized to LF.
//
// The checksum exists to catch a migration edited after it was applied. Line
// endings are not part of that edit: the same commit is checked out with CRLF
// on a Windows host with autocrlf enabled, so a byte-exact hash reports every
// such checkout as a modified migration and refuses to start. Normalizing
// keeps the check about the SQL rather than about the platform that wrote it.
func migrationChecksum(content []byte) string {
	normalized := strings.ReplaceAll(string(content), "\r\n", "\n")
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])
}

// checksumMatches reports whether a recorded checksum describes this migration.
//
// New records use migrationChecksum, but a migration applied by an older build
// was hashed exactly as checked out, which differs between LF and CRLF hosts for
// the same commit. Both of those legacy forms are accepted so an upgrade does
// not demand a database patch; anything else is still a real modification.
func checksumMatches(recorded, sql string) bool {
	if recorded == migrationChecksum([]byte(sql)) {
		return true
	}
	normalized := strings.ReplaceAll(sql, "\r\n", "\n")
	for _, form := range []string{sql, strings.ReplaceAll(normalized, "\n", "\r\n")} {
		sum := sha256.Sum256([]byte(form))
		if recorded == hex.EncodeToString(sum[:]) {
			return true
		}
	}
	return false
}

// PostgresMigrations returns the embedded PostgreSQL migrations.
func PostgresMigrations() ([]Migration, error) {
	return LoadMigrations(postgresMigrations, "migrations/postgres")
}

// ClickHouseMigrations returns the embedded ClickHouse migrations.
func ClickHouseMigrations() ([]Migration, error) {
	return LoadMigrations(clickhouseMigrations, "migrations/clickhouse")
}

// osDirFS exposes a host directory as an fs.FS so the same loader serves both
// embedded and on-disk migrations.
func osDirFS(dir string) (fs.FS, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%q is not a directory", dir)
	}
	return os.DirFS(dir), nil
}

// ResolveMigrations prefers an on-disk directory when one is configured and
// exists, and falls back to the embedded set.
//
// The override exists for operators who manage schema changes through their own
// change-control process and want the files visible in the deployment artifact.
// Falling back to embedded means a missing directory is never fatal.
func ResolveMigrations(dir, subdir string, embedded func() ([]Migration, error)) ([]Migration, error) {
	if dir != "" {
		path := filepath.Join(dir, subdir)
		if fsys, err := osDirFS(path); err == nil {
			migrations, lerr := LoadMigrations(fsys, ".")
			if lerr == nil && len(migrations) > 0 {
				return migrations, nil
			}
		}
	}
	return embedded()
}

// SplitStatements splits a migration file into individual statements.
//
// A naive split on ';' is wrong: semicolons appear inside string literals, inside
// line and block comments, and inside PostgreSQL dollar-quoted function bodies.
// This scanner tracks all four contexts, which is the minimum needed to run the
// migrations in this repository and to stay correct for the ones an operator adds.
func SplitStatements(sql string) []string {
	var (
		statements []string
		current    strings.Builder
	)

	i := 0
	n := len(sql)
	for i < n {
		ch := sql[i]

		switch {
		// Line comment: consume to end of line, preserving it so errors reported
		// by the database still point at the right line.
		case ch == '-' && i+1 < n && sql[i+1] == '-':
			end := strings.IndexByte(sql[i:], '\n')
			if end < 0 {
				current.WriteString(sql[i:])
				i = n
				continue
			}
			current.WriteString(sql[i : i+end+1])
			i += end + 1

		// Block comment: consume through the closing marker. Postgres block
		// comments nest, so a depth counter is used.
		case ch == '/' && i+1 < n && sql[i+1] == '*':
			depth := 1
			j := i + 2
			for j < n && depth > 0 {
				if j+1 < n && sql[j] == '/' && sql[j+1] == '*' {
					depth++
					j += 2
					continue
				}
				if j+1 < n && sql[j] == '*' && sql[j+1] == '/' {
					depth--
					j += 2
					continue
				}
				j++
			}
			current.WriteString(sql[i:j])
			i = j

		// Single-quoted string: a doubled quote is an escaped quote.
		case ch == '\'':
			j := i + 1
			for j < n {
				if sql[j] == '\'' {
					if j+1 < n && sql[j+1] == '\'' {
						j += 2
						continue
					}
					j++
					break
				}
				j++
			}
			current.WriteString(sql[i:j])
			i = j

		// Dollar-quoted string, used for function bodies.
		case ch == '$':
			tag, ok := readDollarTag(sql[i:])
			if !ok {
				current.WriteByte(ch)
				i++
				continue
			}
			closing := tag
			j := i + len(tag)
			end := strings.Index(sql[j:], closing)
			if end < 0 {
				current.WriteString(sql[i:])
				i = n
				continue
			}
			j += end + len(closing)
			current.WriteString(sql[i:j])
			i = j

		case ch == ';':
			stmt := strings.TrimSpace(current.String())
			if stmt != "" {
				statements = append(statements, stmt)
			}
			current.Reset()
			i++

		default:
			current.WriteByte(ch)
			i++
		}
	}

	if tail := strings.TrimSpace(current.String()); tail != "" {
		statements = append(statements, tail)
	}
	return statements
}

// readDollarTag parses a leading dollar-quote tag such as $$ or $body$.
func readDollarTag(s string) (string, bool) {
	if len(s) == 0 || s[0] != '$' {
		return "", false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '$':
			return s[:i+1], true
		case c == '_', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			continue
		default:
			return "", false
		}
	}
	return "", false
}

// MigrationResult reports what a migration run did.
type MigrationResult struct {
	// Applied lists versions applied during this run.
	Applied []string
	// Skipped lists versions already present.
	Skipped []string
	// Duration is the total wall-clock time.
	Duration timeDuration
}

// MigrationError reports a failed migration with the statement that failed.
type MigrationError struct {
	Version string
	// Statement is the failing statement, truncated for readability.
	Statement string
	Err       error
}

// Error implements the error interface.
func (e *MigrationError) Error() string {
	return fmt.Sprintf("migration %s failed: %v\nstatement: %s", e.Version, e.Err, truncate(e.Statement, 400))
}

// Unwrap exposes the underlying error.
func (e *MigrationError) Unwrap() error { return e.Err }

// truncate shortens a string for an error message.
func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// requireMigrations returns an error when no migrations were found, which almost
// always means a broken build rather than an intentionally empty directory.
func requireMigrations(migrations []Migration, engine string) error {
	if len(migrations) == 0 {
		return domain.Errorf(domain.ErrCodeInternal, "no %s migrations were found", engine)
	}
	return nil
}
