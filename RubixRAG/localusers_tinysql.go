package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	tinysql "github.com/SimonWaldherr/tinySQL"
)

// tinySQLLocalUserStore is the tinySQL-backed localUserStore — its own
// *tinysql.DB instance/file (storageSettings.UsersPath), entirely separate
// from the chunk store's tinySQLStore (vectorstore_tinysql.go), same
// reasoning as sqliteLocalUserStore (see localusers.go's package comment).
// Always opened in ModeDisk regardless of the chunk store's configured
// Mode: the users table is tiny and read far more than written, so none of
// vectorstore_tinysql.go's memory-budget tradeoffs around Mode apply here —
// disk mode with no MaxMemoryBytes ceiling is simplest and always correct.
type tinySQLLocalUserStore struct {
	db   *tinysql.DB
	dbMu sync.Mutex
}

func newTinySQLLocalUserStore(cfg storageSettings) (*tinySQLLocalUserStore, error) {
	path := cfg.UsersPath
	if path == "" {
		path = "r3-users-tinysql"
	}
	db, err := tinysql.OpenDB(tinysql.StorageConfig{
		Mode: tinysql.ModeDisk,
		Path: path,
	})
	if err != nil {
		return nil, fmt.Errorf("open tinySQL local user store (path=%s): %w", path, err)
	}
	return &tinySQLLocalUserStore{db: db}, nil
}

// Close releases the underlying tinySQL handle — same test-only reasoning
// as sqliteLocalUserStore.Close.
func (s *tinySQLLocalUserStore) Close() error {
	return s.db.Close()
}

// exec mirrors tinySQLStore's own exec (vectorstore_tinysql.go) — every
// method funnels raw SQL text through here, serialized under dbMu since
// tinySQL is single-writer.
func (s *tinySQLLocalUserStore) exec(q string) (*tinysql.ResultSet, error) {
	stmt, err := tinysql.ParseSQL(q)
	if err != nil {
		return nil, fmt.Errorf("parse %q: %w", q, err)
	}
	s.dbMu.Lock()
	defer s.dbMu.Unlock()
	return tinysql.Execute(context.Background(), s.db, "default", stmt)
}

func (s *tinySQLLocalUserStore) init() error {
	_, err := s.exec(`CREATE TABLE IF NOT EXISTS local_users (
		id TEXT, username TEXT, password_hash TEXT, display_name TEXT,
		email TEXT, department TEXT, dept_code TEXT, is_admin INT,
		disabled INT, created_at INT, updated_at INT
	)`)
	return err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func rowToLocalUser(row tinysql.Row) localUser {
	get := func(col string) any { v, _ := tinysql.GetVal(row, col); return v }
	return localUser{
		ID:           toStr(get("id")),
		Username:     toStr(get("username")),
		PasswordHash: toStr(get("password_hash")),
		DisplayName:  toStr(get("display_name")),
		Email:        toStr(get("email")),
		Department:   toStr(get("department")),
		DeptCode:     toStr(get("dept_code")),
		IsAdmin:      toInt(get("is_admin")) != 0,
		Disabled:     toInt(get("disabled")) != 0,
		CreatedAt:    int64(toInt(get("created_at"))),
		UpdatedAt:    int64(toInt(get("updated_at"))),
	}
}

func (s *tinySQLLocalUserStore) create(u localUser) error {
	// tinySQL has no unique-index support (unlike sqliteLocalUserStore's
	// UNIQUE INDEX ... COLLATE NOCASE) — enforce uniqueness here instead,
	// so a duplicate username fails the same way regardless of backend.
	if _, exists, err := s.getByUsername(u.Username); err != nil {
		return err
	} else if exists {
		return fmt.Errorf("a user with this username already exists")
	}
	if u.ID == "" {
		u.ID = newLocalUserID()
	}
	now := time.Now().Unix()
	q := fmt.Sprintf(
		`INSERT INTO local_users VALUES (%s, %s, %s, %s, %s, %s, %s, %d, %d, %d, %d)`,
		sqlStr(u.ID), sqlStr(normalizeUsername(u.Username)), sqlStr(u.PasswordHash), sqlStr(u.DisplayName),
		sqlStr(u.Email), sqlStr(u.Department), sqlStr(u.DeptCode), boolToInt(u.IsAdmin), boolToInt(u.Disabled), now, now,
	)
	_, err := s.exec(q)
	if err != nil {
		return fmt.Errorf("create local user: %w", err)
	}
	return nil
}

func (s *tinySQLLocalUserStore) getByUsername(username string) (localUser, bool, error) {
	rs, err := s.exec(fmt.Sprintf(`SELECT * FROM local_users WHERE LOWER(username) = %s LIMIT 1`, sqlStr(normalizeUsername(username))))
	if err != nil {
		return localUser{}, false, fmt.Errorf("get local user by username: %w", err)
	}
	if rs == nil || len(rs.Rows) == 0 {
		return localUser{}, false, nil
	}
	return rowToLocalUser(rs.Rows[0]), true, nil
}

func (s *tinySQLLocalUserStore) getByID(id string) (localUser, bool, error) {
	rs, err := s.exec(fmt.Sprintf(`SELECT * FROM local_users WHERE id = %s LIMIT 1`, sqlStr(id)))
	if err != nil {
		return localUser{}, false, fmt.Errorf("get local user by id: %w", err)
	}
	if rs == nil || len(rs.Rows) == 0 {
		return localUser{}, false, nil
	}
	return rowToLocalUser(rs.Rows[0]), true, nil
}

func (s *tinySQLLocalUserStore) list() ([]localUser, error) {
	rs, err := s.exec(`SELECT * FROM local_users`)
	if err != nil {
		return nil, fmt.Errorf("list local users: %w", err)
	}
	out := make([]localUser, 0, len(rs.Rows))
	for _, row := range rs.Rows {
		out = append(out, rowToLocalUser(row))
	}
	sortLocalUsersByUsername(out)
	return out, nil
}

func (s *tinySQLLocalUserStore) update(u localUser) error {
	if _, ok, err := s.getByID(u.ID); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("no such local user")
	}
	q := fmt.Sprintf(
		`UPDATE local_users SET username = %s, display_name = %s, email = %s, department = %s, dept_code = %s, is_admin = %d, disabled = %d, updated_at = %d WHERE id = %s`,
		sqlStr(normalizeUsername(u.Username)), sqlStr(u.DisplayName), sqlStr(u.Email), sqlStr(u.Department), sqlStr(u.DeptCode),
		boolToInt(u.IsAdmin), boolToInt(u.Disabled), time.Now().Unix(), sqlStr(u.ID),
	)
	_, err := s.exec(q)
	if err != nil {
		return fmt.Errorf("update local user: %w", err)
	}
	return nil
}

func (s *tinySQLLocalUserStore) setPassword(id, passwordHash string) error {
	if _, ok, err := s.getByID(id); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("no such local user")
	}
	q := fmt.Sprintf(`UPDATE local_users SET password_hash = %s, updated_at = %d WHERE id = %s`, sqlStr(passwordHash), time.Now().Unix(), sqlStr(id))
	_, err := s.exec(q)
	if err != nil {
		return fmt.Errorf("set local user password: %w", err)
	}
	return nil
}

func (s *tinySQLLocalUserStore) delete(id string) error {
	if _, ok, err := s.getByID(id); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("no such local user")
	}
	_, err := s.exec(fmt.Sprintf(`DELETE FROM local_users WHERE id = %s`, sqlStr(id)))
	if err != nil {
		return fmt.Errorf("delete local user: %w", err)
	}
	return nil
}
