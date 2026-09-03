package main

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// sqliteLocalUserStore is the SQLite-backed localUserStore — see
// localusers.go's package comment for why this is its own file/connection
// rather than reusing sqliteStore's (vectorstore_sqlite.go).
type sqliteLocalUserStore struct {
	db *sql.DB
}

// newSQLiteLocalUserStore opens (creating if needed) the SQLite file at
// cfg.UsersPath, defaulting to "r3-users.db" — same WAL/busy-timeout/
// single-connection shape as newSQLiteStore, for the same SQLITE_BUSY
// reasoning.
func newSQLiteLocalUserStore(cfg storageSettings) (*sqliteLocalUserStore, error) {
	path := cfg.UsersPath
	if path == "" {
		path = "r3-users.db"
	}
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite local user store: %w", err)
	}
	db.SetMaxOpenConns(1)
	return &sqliteLocalUserStore{db: db}, nil
}

// Close releases the underlying file handle — not part of localUserStore
// (the process-wide localUsers store lives for the process's lifetime),
// only used by tests that need the file releasable before t.TempDir()
// cleanup, same reasoning as sqliteStore.Close (vectorstore_sqlite.go).
func (s *sqliteLocalUserStore) Close() error {
	return s.db.Close()
}

func (s *sqliteLocalUserStore) init() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS local_users (
		id TEXT PRIMARY KEY,
		username TEXT NOT NULL,
		password_hash TEXT NOT NULL,
		display_name TEXT NOT NULL,
		email TEXT NOT NULL,
		department TEXT NOT NULL,
		dept_code TEXT NOT NULL,
		is_admin INTEGER NOT NULL,
		disabled INTEGER NOT NULL,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	)`)
	if err != nil {
		return fmt.Errorf("init local_users schema: %w", err)
	}
	_, err = s.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_local_users_username ON local_users(username COLLATE NOCASE)`)
	if err != nil {
		return fmt.Errorf("init local_users username index: %w", err)
	}
	return nil
}

func (s *sqliteLocalUserStore) create(u localUser) error {
	if u.ID == "" {
		u.ID = newLocalUserID()
	}
	now := time.Now().Unix()
	_, err := s.db.Exec(
		`INSERT INTO local_users (id, username, password_hash, display_name, email, department, dept_code, is_admin, disabled, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		u.ID, normalizeUsername(u.Username), u.PasswordHash, u.DisplayName, u.Email, u.Department, u.DeptCode, u.IsAdmin, u.Disabled, now, now,
	)
	if err != nil {
		return fmt.Errorf("create local user: %w", err)
	}
	return nil
}

func scanLocalUser(row interface{ Scan(...any) error }) (localUser, error) {
	var u localUser
	var isAdmin, disabled int
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.DisplayName, &u.Email, &u.Department, &u.DeptCode, &isAdmin, &disabled, &u.CreatedAt, &u.UpdatedAt)
	u.IsAdmin = isAdmin != 0
	u.Disabled = disabled != 0
	return u, err
}

const localUserSelectColumns = `id, username, password_hash, display_name, email, department, dept_code, is_admin, disabled, created_at, updated_at`

func (s *sqliteLocalUserStore) getByUsername(username string) (localUser, bool, error) {
	row := s.db.QueryRow(`SELECT `+localUserSelectColumns+` FROM local_users WHERE username = ? COLLATE NOCASE`, normalizeUsername(username))
	u, err := scanLocalUser(row)
	if err == sql.ErrNoRows {
		return localUser{}, false, nil
	}
	if err != nil {
		return localUser{}, false, fmt.Errorf("get local user by username: %w", err)
	}
	return u, true, nil
}

func (s *sqliteLocalUserStore) getByID(id string) (localUser, bool, error) {
	row := s.db.QueryRow(`SELECT `+localUserSelectColumns+` FROM local_users WHERE id = ?`, id)
	u, err := scanLocalUser(row)
	if err == sql.ErrNoRows {
		return localUser{}, false, nil
	}
	if err != nil {
		return localUser{}, false, fmt.Errorf("get local user by id: %w", err)
	}
	return u, true, nil
}

func (s *sqliteLocalUserStore) list() ([]localUser, error) {
	rows, err := s.db.Query(`SELECT ` + localUserSelectColumns + ` FROM local_users ORDER BY username ASC`)
	if err != nil {
		return nil, fmt.Errorf("list local users: %w", err)
	}
	defer rows.Close()
	var out []localUser
	for rows.Next() {
		u, err := scanLocalUser(rows)
		if err != nil {
			return nil, fmt.Errorf("scan local user: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *sqliteLocalUserStore) update(u localUser) error {
	res, err := s.db.Exec(
		`UPDATE local_users SET username = ?, display_name = ?, email = ?, department = ?, dept_code = ?, is_admin = ?, disabled = ?, updated_at = ?
		 WHERE id = ?`,
		normalizeUsername(u.Username), u.DisplayName, u.Email, u.Department, u.DeptCode, u.IsAdmin, u.Disabled, time.Now().Unix(), u.ID,
	)
	if err != nil {
		return fmt.Errorf("update local user: %w", err)
	}
	return rowsAffectedOrNotFound(res)
}

func (s *sqliteLocalUserStore) setPassword(id, passwordHash string) error {
	res, err := s.db.Exec(`UPDATE local_users SET password_hash = ?, updated_at = ? WHERE id = ?`, passwordHash, time.Now().Unix(), id)
	if err != nil {
		return fmt.Errorf("set local user password: %w", err)
	}
	return rowsAffectedOrNotFound(res)
}

func (s *sqliteLocalUserStore) delete(id string) error {
	res, err := s.db.Exec(`DELETE FROM local_users WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete local user: %w", err)
	}
	return rowsAffectedOrNotFound(res)
}

// rowsAffectedOrNotFound turns a zero-rows-affected UPDATE/DELETE into an
// explicit error, so callers (handlers_local_users.go) can tell "no such
// user id" apart from a silent no-op.
func rowsAffectedOrNotFound(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("no such local user")
	}
	return nil
}
