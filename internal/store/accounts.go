package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

type Admin struct {
	ID           int64
	Username     string
	Email        string
	PasswordHash string
	CreatedAt    time.Time
	CreatedBy    string
	LastLoginAt  *time.Time
}

const adminCols = `id, username, email, password_hash, created_at, created_by, last_login_at`

func scanAdmin(row interface{ Scan(...any) error }) (*Admin, error) {
	var a Admin
	var created string
	var last sql.NullString
	if err := row.Scan(&a.ID, &a.Username, &a.Email, &a.PasswordHash, &created, &a.CreatedBy, &last); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	a.CreatedAt = parseTS(created)
	a.LastLoginAt = parseNullTS(last)
	return &a, nil
}

func (s *Store) CreateAdmin(username, email, hash, createdBy string) (*Admin, error) {
	res, err := s.db.Exec(`INSERT INTO admins(username, email, password_hash, created_at, created_by) VALUES (?, ?, ?, ?, ?)`,
		username, email, hash, now(), createdBy)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, ErrExists
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.AdminByID(id)
}

func (s *Store) AdminByID(id int64) (*Admin, error) {
	return scanAdmin(s.db.QueryRow(`SELECT `+adminCols+` FROM admins WHERE id = ?`, id))
}

func (s *Store) AdminByUsername(username string) (*Admin, error) {
	return scanAdmin(s.db.QueryRow(`SELECT `+adminCols+` FROM admins WHERE username = ?`, username))
}

func (s *Store) Admins() ([]Admin, error) {
	rows, err := s.db.Query(`SELECT ` + adminCols + ` FROM admins ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Admin
	for rows.Next() {
		a, err := scanAdmin(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

func (s *Store) CountAdmins() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT count(*) FROM admins`).Scan(&n)
	return n, err
}

// SetAdminPassword changes the hash and ends the account's sessions except keepToken.
func (s *Store) SetAdminPassword(id int64, hash, keepTokenHash string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE admins SET password_hash = ? WHERE id = ?`, hash, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(`DELETE FROM sessions WHERE admin_id = ? AND token_hash != ?`, id, keepTokenHash); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SetAdminEmail(id int64, email string) error {
	_, err := s.db.Exec(`UPDATE admins SET email = ? WHERE id = ?`, email, id)
	return err
}

func (s *Store) DeleteAdmin(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM admins`).Scan(&n); err != nil {
		return err
	}
	if n <= 1 {
		return ErrLastAdmin
	}
	res, err := tx.Exec(`DELETE FROM admins WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

func (s *Store) TouchAdminLogin(id int64) {
	s.db.Exec(`UPDATE admins SET last_login_at = ? WHERE id = ?`, now(), id)
}

// AdminEmails returns the notification addresses of all admins that have one.
func (s *Store) AdminEmails() ([]string, error) {
	rows, err := s.db.Query(`SELECT email FROM admins WHERE email != '' ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Sessions. An admin session has AdminID set; a guest session has InviteID
// and Email set and can only work on that invite's draft and submit it.
const (
	SessionAdmin = "admin"
	SessionGuest = "guest"
)

type Session struct {
	Kind      string
	AdminID   int64
	InviteID  int64
	Email     string
	ExpiresAt time.Time
}

func (s *Store) CreateSession(tokenHash string, sess Session, ttl time.Duration) error {
	t := time.Now()
	var adminID, inviteID any
	if sess.Kind == SessionAdmin {
		adminID = sess.AdminID
	} else {
		inviteID = sess.InviteID
	}
	_, err := s.db.Exec(`INSERT INTO sessions(token_hash, kind, admin_id, invite_id, email, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		tokenHash, sess.Kind, adminID, inviteID, sess.Email, ts(t), ts(t.Add(ttl)))
	return err
}

func (s *Store) SessionByToken(tokenHash string) (*Session, error) {
	var sess Session
	var adminID, inviteID sql.NullInt64
	var exp string
	err := s.db.QueryRow(`SELECT kind, admin_id, invite_id, email, expires_at FROM sessions WHERE token_hash = ? AND expires_at > ?`,
		tokenHash, now()).Scan(&sess.Kind, &adminID, &inviteID, &sess.Email, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	sess.AdminID, sess.InviteID = adminID.Int64, inviteID.Int64
	sess.ExpiresAt = parseTS(exp)
	return &sess, nil
}

func (s *Store) DeleteSession(tokenHash string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
	return err
}

func (s *Store) DeleteGuestSessions(inviteID int64, email string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE invite_id = ? AND email = ?`, inviteID, email)
	return err
}

func (s *Store) PurgeExpiredSessions() error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE expires_at <= ?`, now())
	return err
}
