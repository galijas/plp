// Package store keeps everything except uploaded file contents in SQLite:
// admins, sessions, invite codes, drafts, upload records, form versions,
// submissions, the action log and settings.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

var (
	ErrNotFound  = errors.New("not found")
	ErrExists    = errors.New("already exists")
	ErrConflict  = errors.New("changed by someone else")
	ErrLastAdmin = errors.New("the last admin account can't be deleted")
)

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

var migrations = []string{
	`CREATE TABLE admins (
		id            INTEGER PRIMARY KEY,
		username      TEXT NOT NULL UNIQUE COLLATE NOCASE,
		email         TEXT NOT NULL DEFAULT '',
		password_hash TEXT NOT NULL,
		created_at    TEXT NOT NULL,
		created_by    TEXT NOT NULL DEFAULT '',
		last_login_at TEXT
	);
	CREATE TABLE invites (
		id          INTEGER PRIMARY KEY,
		code        TEXT NOT NULL UNIQUE,
		email       TEXT NOT NULL DEFAULT '',
		label       TEXT NOT NULL DEFAULT '',
		max_uses    INTEGER NOT NULL DEFAULT 0,
		uses        INTEGER NOT NULL DEFAULT 0,
		expires_at  TEXT,
		revoked_at  TEXT,
		prefill     TEXT NOT NULL DEFAULT '{}',
		created_at  TEXT NOT NULL,
		created_by  TEXT NOT NULL
	);
	CREATE TABLE sessions (
		token_hash TEXT PRIMARY KEY,
		kind       TEXT NOT NULL,
		admin_id   INTEGER REFERENCES admins(id) ON DELETE CASCADE,
		invite_id  INTEGER REFERENCES invites(id) ON DELETE CASCADE,
		email      TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		expires_at TEXT NOT NULL
	);
	CREATE TABLE drafts (
		invite_id  INTEGER NOT NULL REFERENCES invites(id) ON DELETE CASCADE,
		email      TEXT NOT NULL,
		answers    TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		PRIMARY KEY (invite_id, email)
	);
	CREATE TABLE uploads (
		id           TEXT PRIMARY KEY,
		invite_id    INTEGER NOT NULL REFERENCES invites(id) ON DELETE CASCADE,
		email        TEXT NOT NULL,
		item_id      TEXT NOT NULL,
		name         TEXT NOT NULL,
		size         INTEGER NOT NULL,
		received     INTEGER NOT NULL DEFAULT 0,
		created_at   TEXT NOT NULL,
		completed_at TEXT
	);
	CREATE INDEX uploads_owner ON uploads(invite_id, email);
	CREATE TABLE form_versions (
		id         INTEGER PRIMARY KEY,
		json       TEXT NOT NULL,
		created_at TEXT NOT NULL,
		created_by TEXT NOT NULL
	);
	CREATE TABLE submissions (
		id           TEXT PRIMARY KEY,
		invite_id    INTEGER REFERENCES invites(id) ON DELETE SET NULL,
		invite_code  TEXT NOT NULL,
		email        TEXT NOT NULL,
		label        TEXT NOT NULL DEFAULT '',
		submitted_at TEXT NOT NULL,
		form_version INTEGER NOT NULL,
		snapshot     TEXT NOT NULL,
		folder       TEXT NOT NULL UNIQUE,
		files_count  INTEGER NOT NULL DEFAULT 0,
		files_bytes  INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX submissions_time ON submissions(submitted_at);
	CREATE TABLE audit (
		id         INTEGER PRIMARY KEY,
		at         TEXT NOT NULL,
		actor_kind TEXT NOT NULL,
		actor      TEXT NOT NULL,
		ip         TEXT NOT NULL DEFAULT '',
		action     TEXT NOT NULL,
		detail     TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX audit_at ON audit(at);
	CREATE TABLE settings (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);`,

	// 2: invite codes no longer pre-fill form answers (the invite link
	// fills in the code and email instead).
	`ALTER TABLE invites DROP COLUMN prefill;`,

	// 3: upload limits per question (hook below).
	"",
}

// migrationHooks run after a migration's SQL, in the same transaction.
var migrationHooks = map[int]func(*sql.Tx) error{
	3: applyUploadLimits,
}

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return err
	}
	var v int
	err := s.db.QueryRow(`SELECT version FROM schema_version`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := s.db.Exec(`INSERT INTO schema_version VALUES (0)`); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if v > len(migrations) {
		return fmt.Errorf("database schema %d is newer than this program (%d)", v, len(migrations))
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(context.Background(), nil)
		if err != nil {
			return err
		}
		if migrations[i] != "" {
			if _, err := tx.Exec(migrations[i]); err != nil {
				tx.Rollback()
				return fmt.Errorf("migration %d: %w", i+1, err)
			}
		}
		if hook := migrationHooks[i+1]; hook != nil {
			if err := hook(tx); err != nil {
				tx.Rollback()
				return fmt.Errorf("migration %d: %w", i+1, err)
			}
		}
		if _, err := tx.Exec(`UPDATE schema_version SET version = ?`, i+1); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Times are stored as RFC 3339 UTC text, which sorts correctly.
const tsLayout = time.RFC3339

func ts(t time.Time) string { return t.UTC().Format(tsLayout) }

func now() string { return ts(time.Now()) }

func parseTS(s string) time.Time {
	t, _ := time.Parse(tsLayout, s)
	return t
}

func parseNullTS(ns sql.NullString) *time.Time {
	if !ns.Valid || ns.String == "" {
		return nil
	}
	t := parseTS(ns.String)
	return &t
}

func nullTS(t *time.Time) any {
	if t == nil {
		return nil
	}
	return ts(*t)
}

// Backup writes a consistent copy of the database to path.
func (s *Store) Backup(path string) error {
	_, err := s.db.Exec(`VACUUM INTO ?`, path)
	return err
}

func (s *Store) Setting(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings(key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}
