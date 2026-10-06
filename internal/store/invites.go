package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

type Invite struct {
	ID        int64
	Code      string
	Email     string // empty: any email may use the code
	Label     string
	MaxUses   int // 0: unlimited until expiry
	Uses      int
	ExpiresAt *time.Time
	RevokedAt *time.Time
	Prefill   map[string]string // item ID -> text pre-filled into new drafts
	CreatedAt time.Time
	CreatedBy string
}

// Invite states, as shown in the admin panel.
const (
	InviteActive  = "active"
	InviteExpired = "expired"
	InviteRevoked = "revoked"
	InviteUsed    = "used"
)

func (inv *Invite) Status() string {
	switch {
	case inv.RevokedAt != nil:
		return InviteRevoked
	case inv.ExpiresAt != nil && !time.Now().Before(*inv.ExpiresAt):
		return InviteExpired
	case inv.MaxUses > 0 && inv.Uses >= inv.MaxUses:
		return InviteUsed
	}
	return InviteActive
}

// AllowsEmail reports whether email (already lowercased) may use the code.
func (inv *Invite) AllowsEmail(email string) bool {
	return inv.Email == "" || strings.EqualFold(inv.Email, email)
}

const inviteCols = `id, code, email, label, max_uses, uses, expires_at, revoked_at, prefill, created_at, created_by`

func scanInvite(row interface{ Scan(...any) error }) (*Invite, error) {
	var inv Invite
	var exp, rev sql.NullString
	var prefill, created string
	if err := row.Scan(&inv.ID, &inv.Code, &inv.Email, &inv.Label, &inv.MaxUses, &inv.Uses, &exp, &rev, &prefill, &created, &inv.CreatedBy); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	inv.ExpiresAt, inv.RevokedAt = parseNullTS(exp), parseNullTS(rev)
	inv.CreatedAt = parseTS(created)
	json.Unmarshal([]byte(prefill), &inv.Prefill)
	if inv.Prefill == nil {
		inv.Prefill = map[string]string{}
	}
	return &inv, nil
}

func (s *Store) CreateInvite(inv *Invite) error {
	pf, _ := json.Marshal(inv.Prefill)
	if inv.Prefill == nil {
		pf = []byte("{}")
	}
	inv.CreatedAt = time.Now()
	res, err := s.db.Exec(`INSERT INTO invites(code, email, label, max_uses, expires_at, prefill, created_at, created_by) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		inv.Code, inv.Email, inv.Label, inv.MaxUses, nullTS(inv.ExpiresAt), string(pf), ts(inv.CreatedAt), inv.CreatedBy)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return ErrExists
		}
		return err
	}
	inv.ID, _ = res.LastInsertId()
	return nil
}

func (s *Store) InviteByID(id int64) (*Invite, error) {
	return scanInvite(s.db.QueryRow(`SELECT `+inviteCols+` FROM invites WHERE id = ?`, id))
}

func (s *Store) InviteByCode(code string) (*Invite, error) {
	return scanInvite(s.db.QueryRow(`SELECT `+inviteCols+` FROM invites WHERE code = ?`, code))
}

func (s *Store) Invites() ([]Invite, error) {
	rows, err := s.db.Query(`SELECT ` + inviteCols + ` FROM invites ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Invite
	for rows.Next() {
		inv, err := scanInvite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *inv)
	}
	return out, rows.Err()
}

// UpdateInvite saves the editable fields: label, email, uses, expiry and prefill.
func (s *Store) UpdateInvite(inv *Invite) error {
	pf, _ := json.Marshal(inv.Prefill)
	res, err := s.db.Exec(`UPDATE invites SET label = ?, email = ?, max_uses = ?, expires_at = ?, prefill = ? WHERE id = ?`,
		inv.Label, inv.Email, inv.MaxUses, nullTS(inv.ExpiresAt), string(pf), inv.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetInviteRevoked revokes (ending its guest sessions) or restores a code.
func (s *Store) SetInviteRevoked(id int64, revoked bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var at any
	if revoked {
		at = now()
	}
	res, err := tx.Exec(`UPDATE invites SET revoked_at = ? WHERE id = ?`, at, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if revoked {
		if _, err := tx.Exec(`DELETE FROM sessions WHERE invite_id = ?`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteInvite removes a code with its drafts and staged uploads; it returns
// the staged upload IDs so their files can be removed. Submissions made
// with the code are kept.
func (s *Store) DeleteInvite(id int64) ([]string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT id FROM uploads WHERE invite_id = ?`, id)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var u string
		rows.Scan(&u)
		ids = append(ids, u)
	}
	rows.Close()
	res, err := tx.Exec(`DELETE FROM invites WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	return ids, tx.Commit()
}

// InviteSubmissionCounts returns the number of submissions per invite ID.
func (s *Store) InviteSubmissionCounts() (map[int64]int, error) {
	rows, err := s.db.Query(`SELECT invite_id, count(*) FROM submissions WHERE invite_id IS NOT NULL GROUP BY invite_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var id int64
		var n int
		rows.Scan(&id, &n)
		out[id] = n
	}
	return out, rows.Err()
}
