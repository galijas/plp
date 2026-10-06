package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"plportal/internal/formdef"
)

// CurrentForm returns the latest form version, creating version 1 from the
// built-in default on first use.
func (s *Store) CurrentForm() (formdef.Form, int64, error) {
	var id int64
	var js string
	err := s.db.QueryRow(`SELECT id, json FROM form_versions ORDER BY id DESC LIMIT 1`).Scan(&id, &js)
	if errors.Is(err, sql.ErrNoRows) {
		f := formdef.Default()
		b, _ := json.Marshal(f)
		res, err := s.db.Exec(`INSERT INTO form_versions(json, created_at, created_by) VALUES (?, ?, 'system')`, string(b), now())
		if err != nil {
			return f, 0, err
		}
		id, _ = res.LastInsertId()
		return f, id, nil
	}
	if err != nil {
		return formdef.Form{}, 0, err
	}
	var f formdef.Form
	err = json.Unmarshal([]byte(js), &f)
	return f, id, err
}

// SaveForm stores a new version if the current one is still baseVersion.
func (s *Store) SaveForm(f formdef.Form, baseVersion int64, by string) (int64, error) {
	b, err := json.Marshal(f)
	if err != nil {
		return 0, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var cur int64
	if err := tx.QueryRow(`SELECT coalesce(max(id), 0) FROM form_versions`).Scan(&cur); err != nil {
		return 0, err
	}
	if cur != baseVersion {
		return 0, ErrConflict
	}
	res, err := tx.Exec(`INSERT INTO form_versions(json, created_at, created_by) VALUES (?, ?, ?)`, string(b), now(), by)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, tx.Commit()
}

// Drafts: one per invite and email, kept until submitted.

func (s *Store) Draft(inviteID int64, email string) (formdef.Answers, *time.Time, error) {
	var js, at string
	err := s.db.QueryRow(`SELECT answers, updated_at FROM drafts WHERE invite_id = ? AND email = ?`, inviteID, email).Scan(&js, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var a formdef.Answers
	json.Unmarshal([]byte(js), &a)
	t := parseTS(at)
	return a, &t, nil
}

func (s *Store) SaveDraft(inviteID int64, email string, a formdef.Answers) (time.Time, error) {
	b, _ := json.Marshal(a)
	t := time.Now()
	_, err := s.db.Exec(`INSERT INTO drafts(invite_id, email, answers, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(invite_id, email) DO UPDATE SET answers = excluded.answers, updated_at = excluded.updated_at`,
		inviteID, email, string(b), ts(t))
	return t, err
}

// Uploads: files staged by a guest for a draft. Received counts the bytes
// written so far; a file is complete when Received == Size.

type Upload struct {
	ID          string
	InviteID    int64
	Email       string
	ItemID      string
	Name        string
	Size        int64
	Received    int64
	CreatedAt   time.Time
	CompletedAt *time.Time
}

func (u *Upload) Complete() bool { return u.CompletedAt != nil }

const uploadCols = `id, invite_id, email, item_id, name, size, received, created_at, completed_at`

func scanUpload(row interface{ Scan(...any) error }) (*Upload, error) {
	var u Upload
	var created string
	var done sql.NullString
	if err := row.Scan(&u.ID, &u.InviteID, &u.Email, &u.ItemID, &u.Name, &u.Size, &u.Received, &created, &done); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	u.CreatedAt, u.CompletedAt = parseTS(created), parseNullTS(done)
	return &u, nil
}

func (s *Store) CreateUpload(u *Upload) error {
	u.CreatedAt = time.Now()
	_, err := s.db.Exec(`INSERT INTO uploads(id, invite_id, email, item_id, name, size, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		u.ID, u.InviteID, u.Email, u.ItemID, u.Name, u.Size, ts(u.CreatedAt))
	return err
}

func (s *Store) UploadByID(id string) (*Upload, error) {
	return scanUpload(s.db.QueryRow(`SELECT `+uploadCols+` FROM uploads WHERE id = ?`, id))
}

func (s *Store) Uploads(inviteID int64, email string) ([]Upload, error) {
	rows, err := s.db.Query(`SELECT `+uploadCols+` FROM uploads WHERE invite_id = ? AND email = ? ORDER BY created_at, id`, inviteID, email)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Upload
	for rows.Next() {
		u, err := scanUpload(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

func (s *Store) SetUploadReceived(id string, received int64, complete bool) error {
	var done any
	if complete {
		done = now()
	}
	_, err := s.db.Exec(`UPDATE uploads SET received = ?, completed_at = ? WHERE id = ?`, received, done, id)
	return err
}

func (s *Store) DeleteUpload(id string) error {
	_, err := s.db.Exec(`DELETE FROM uploads WHERE id = ?`, id)
	return err
}

// StaleUploads returns incomplete uploads not touched since before.
func (s *Store) StaleUploads(before time.Time) ([]string, error) {
	rows, err := s.db.Query(`SELECT id FROM uploads WHERE completed_at IS NULL AND created_at < ?`, ts(before))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// AllUploadIDs is used to find orphaned files in the staging directory.
func (s *Store) AllUploadIDs() (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT id FROM uploads`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		rows.Scan(&id)
		out[id] = true
	}
	return out, rows.Err()
}
