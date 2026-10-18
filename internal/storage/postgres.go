package storage

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shadowsafin/synapass/internal/config"
	"github.com/shadowsafin/synapass/internal/domain"
)

// timeDuration aliases time.Duration so MigrationResult can be declared before
// the time import is used elsewhere in the file.
type timeDuration = time.Duration

// Postgres owns the connection pool to the system of record.
//
// A pool is used rather than a single connection because the gateway is
// concurrent and Postgres connections are relatively expensive. The pool is
// sized from configuration and its health is observable, so an operator can see
// saturation before it becomes an outage.
type Postgres struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
	cfg    config.DatabaseConfig
}

// NewPostgres opens a connection pool and verifies connectivity.
//
// Connectivity is verified eagerly so a misconfigured deployment fails at startup
// with a clear message rather than on the first request with a confusing timeout.
func NewPostgres(ctx context.Context, cfg config.DatabaseConfig, logger *slog.Logger) (*Postgres, error) {
	if cfg.DSN == "" {
		return nil, fmt.Errorf("database DSN is empty")
	}
	if logger == nil {
		logger = slog.Default()
	}

	poolCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse database DSN: %w", err)
	}

	// Pool sizing. MaxConns is the ceiling the database must be able to serve
	// across every replica, so it is kept conservative by default.
	poolCfg.MaxConns = cfg.MaxConns
	poolCfg.MinConns = cfg.MinConns
	if cfg.MaxConnLifetime > 0 {
		poolCfg.MaxConnLifetime = cfg.MaxConnLifetime.Std()
	}
	if cfg.MaxConnIdleTime > 0 {
		poolCfg.MaxConnIdleTime = cfg.MaxConnIdleTime.Std()
	}
	if cfg.ConnectTimeout > 0 {
		poolCfg.ConnConfig.ConnectTimeout = cfg.ConnectTimeout.Std()
	}

	// default_transaction_read_only is left alone, but the statement timeout is
	// pushed down to the server so a runaway analytical query cannot pin a
	// connection indefinitely. The gateway's own query timeout is a backstop.
	if cfg.StatementTimeout > 0 {
		ms := cfg.StatementTimeout.Std().Milliseconds()
		if poolCfg.ConnConfig.RuntimeParams == nil {
			poolCfg.ConnConfig.RuntimeParams = map[string]string{}
		}
		poolCfg.ConnConfig.RuntimeParams["statement_timeout"] = fmt.Sprintf("%d", ms)
		poolCfg.ConnConfig.RuntimeParams["application_name"] = "synapass"
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create database pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to database: %w", err)
	}

	logger.Info("connected to postgres",
		"host", cfg.Host,
		"database", cfg.Name,
		"max_conns", cfg.MaxConns,
	)

	return &Postgres{pool: pool, logger: logger, cfg: cfg}, nil
}

// Pool exposes the underlying pool for repositories.
func (p *Postgres) Pool() *pgxpool.Pool { return p.pool }

// Close releases the pool.
func (p *Postgres) Close() {
	if p != nil && p.pool != nil {
		p.pool.Close()
	}
}

// Ping verifies connectivity, used by the health endpoint.
func (p *Postgres) Ping(ctx context.Context) error {
	if p == nil || p.pool == nil {
		return fmt.Errorf("postgres is not initialized")
	}
	return p.pool.Ping(ctx)
}

// Stat renders pool statistics for the metrics endpoint.
func (p *Postgres) Stat() map[string]any {
	if p == nil || p.pool == nil {
		return nil
	}
	s := p.pool.Stat()
	return map[string]any{
		"acquired_conns":   s.AcquiredConns(),
		"idle_conns":       s.IdleConns(),
		"total_conns":      s.TotalConns(),
		"max_conns":        s.MaxConns(),
		"constructing":     s.ConstructingConns(),
		"acquire_count":    s.AcquireCount(),
		"acquire_duration": s.AcquireDuration().String(),
		"empty_acquires":   s.EmptyAcquireCount(),
	}
}

// InTx runs fn inside a transaction, rolling back on error.
//
// A helper is provided rather than letting callers manage transactions inline
// because every repository needs the same rollback-on-error and
// commit-on-success behaviour, and an inverted condition there is a silent data
// corruption bug.
func (p *Postgres) InTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return domain.NewError(domain.ErrCodeInternal, "failed to begin a database transaction").Wrap(err)
	}

	// Rollback after a successful commit is a no-op, so it is safe to defer
	// unconditionally and it guarantees no transaction is left open on an early
	// return.
	defer func() {
		if rbErr := tx.Rollback(ctx); rbErr != nil && rbErr != pgx.ErrTxClosed {
			p.logger.Warn("transaction rollback failed", "error", rbErr)
		}
	}()

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.NewError(domain.ErrCodeInternal, "failed to commit a database transaction").Wrap(err)
	}
	return nil
}

// Migrate applies pending migrations.
//
// Applied migrations are recorded with a checksum. A mismatch on an already
// applied migration aborts the run rather than proceeding, because a difference
// means the schema on disk no longer describes the schema in the database and
// continuing would make the divergence permanent.
func (p *Postgres) Migrate(ctx context.Context) (*MigrationResult, error) {
	start := time.Now()

	migrations, err := ResolveMigrations(p.cfg.MigrationsDir, "postgres", PostgresMigrations)
	if err != nil {
		return nil, err
	}
	if err := requireMigrations(migrations, "postgres"); err != nil {
		return nil, err
	}

	if _, err := p.pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT        PRIMARY KEY,
			checksum   TEXT        NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return nil, domain.NewError(domain.ErrCodeInternal, "failed to create the migrations table").Wrap(err)
	}

	applied, err := p.appliedMigrations(ctx)
	if err != nil {
		return nil, err
	}

	result := &MigrationResult{}
	for _, migration := range migrations {
		if existing, ok := applied[migration.Version]; ok {
			if !checksumMatches(existing, migration.SQL) {
				return nil, domain.Errorf(domain.ErrCodeInternal,
					"migration %s was modified after it was applied (recorded %s, found %s); "+
						"create a new migration instead of editing an applied one",
					migration.Version, existing[:12], migration.Checksum[:12])
			}
			result.Skipped = append(result.Skipped, migration.Version)
			continue
		}

		if err := p.applyMigration(ctx, migration); err != nil {
			return nil, err
		}
		result.Applied = append(result.Applied, migration.Version)
		p.logger.Info("applied migration", "version", migration.Version, "name", migration.Name)
	}

	result.Duration = time.Since(start)
	return result, nil
}

// appliedMigrations returns the recorded versions and their checksums.
func (p *Postgres) appliedMigrations(ctx context.Context) (map[string]string, error) {
	rows, err := p.pool.Query(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return nil, domain.NewError(domain.ErrCodeInternal, "failed to read applied migrations").Wrap(err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var version, checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			return nil, domain.NewError(domain.ErrCodeInternal, "failed to scan an applied migration").Wrap(err)
		}
		out[version] = checksum
	}
	if err := rows.Err(); err != nil {
		return nil, domain.NewError(domain.ErrCodeInternal, "failed to read applied migrations").Wrap(err)
	}
	return out, nil
}

// applyMigration runs one migration's statements in a single transaction, so a
// partially applied migration is never left behind.
func (p *Postgres) applyMigration(ctx context.Context, migration Migration) error {
	statements := SplitStatements(migration.SQL)
	if len(statements) == 0 {
		return nil
	}

	return p.InTx(ctx, func(tx pgx.Tx) error {
		for _, statement := range statements {
			if _, err := tx.Exec(ctx, statement); err != nil {
				return &MigrationError{Version: migration.Version, Statement: statement, Err: err}
			}
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version, checksum) VALUES ($1, $2)
			 ON CONFLICT (version) DO UPDATE SET checksum = EXCLUDED.checksum`,
			migration.Version, migration.Checksum)
		if err != nil {
			return &MigrationError{
				Version:   migration.Version,
				Statement: "record migration",
				Err:       err,
			}
		}
		return nil
	})
}

// MigrationsPending reports whether any migration has not been applied.
//
// The health endpoint uses this to surface "the binary is newer than the schema",
// which is a deployment ordering mistake that otherwise fails confusingly at
// runtime.
func (p *Postgres) MigrationsPending(ctx context.Context) (bool, error) {
	migrations, err := ResolveMigrations(p.cfg.MigrationsDir, "postgres", PostgresMigrations)
	if err != nil {
		return false, err
	}
	applied, err := p.appliedMigrations(ctx)
	if err != nil {
		return false, err
	}
	for _, migration := range migrations {
		if _, ok := applied[migration.Version]; !ok {
			return true, nil
		}
	}
	return false, nil
}
