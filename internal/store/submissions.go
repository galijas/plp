package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"plportal/internal/formdef"
)

type Submission struct {
	ID          string
	InviteID    *int64
	InviteCode  string
	Email       string
	Label       string // first short answer, normally the company name
	SubmittedAt time.Time
	FormVersion int64
	Snapshot    formdef.Snapshot
	Folder      string
	FilesCount  int
	FilesBytes  int64
}

const submissionCols = `id, invite_id, invite_code, email, label, submitted_at, form_version, snapshot, folder, files_count, files_bytes`

func scanSubmission(row interface{ Scan(...any) error }, withSnapshot bool) (*Submission, error) {
	var sub Submission
	var inv sql.NullInt64
	var at, snap string
	if err := row.Scan(&sub.ID, &inv, &sub.InviteCode, &sub.Email, &sub.Label, &at, &sub.FormVersion, &snap, &sub.Folder, &sub.FilesCount, &sub.FilesBytes); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if inv.Valid {
		sub.InviteID = &inv.Int64
	}
	sub.SubmittedAt = parseTS(at)
	if withSnapshot {
		json.Unmarshal([]byte(snap), &sub.Snapshot)
	}
	return &sub, nil
}

var ErrInviteUnusable = errors.New("the invite code is no longer valid")

// FinishSubmission records a submission in one transaction: it uses up one
// use of the invite (failing if the code stopped being valid meanwhile),
// stores the submission, and deletes the draft and the staged upload rows.
// For a code that has no uses left afterwards, the guest's sessions end.
func (s *Store) FinishSubmission(sub *Submission, inviteID int64, uploadIDs []string) error {
	snap, err := json.Marshal(sub.Snapshot)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	t := now()
	res, err := tx.Exec(`UPDATE invites SET uses = uses + 1 WHERE id = ? AND revoked_at IS NULL
		AND (expires_at IS NULL OR expires_at > ?) AND (max_uses = 0 OR uses < max_uses)`, inviteID, t)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrInviteUnusable
	}
	_, err = tx.Exec(`INSERT INTO submissions(`+submissionCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sub.ID, inviteID, sub.InviteCode, sub.Email, sub.Label, ts(sub.SubmittedAt), sub.FormVersion, string(snap),
		sub.Folder, sub.FilesCount, sub.FilesBytes)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM drafts WHERE invite_id = ? AND email = ?`, inviteID, sub.Email); err != nil {
		return err
	}
	for _, id := range uploadIDs {
		if _, err := tx.Exec(`DELETE FROM uploads WHERE id = ?`, id); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM sessions WHERE invite_id = ? AND email = ?
		AND EXISTS (SELECT 1 FROM invites WHERE id = ? AND max_uses > 0 AND uses >= max_uses)`, inviteID, sub.Email, inviteID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SubmissionByID(id string) (*Submission, error) {
	return scanSubmission(s.db.QueryRow(`SELECT `+submissionCols+` FROM submissions WHERE id = ?`, id), true)
}

type SubmissionFilter struct {
	Query  string // matches email, label, code or folder
	From   *time.Time
	To     *time.Time
	Limit  int
	Offset int
}

func (s *Store) Submissions(f SubmissionFilter) ([]Submission, int, error) {
	where := []string{"1=1"}
	var args []any
	if q := strings.TrimSpace(f.Query); q != "" {
		like := "%" + escapeLike(q) + "%"
		where = append(where, `(email LIKE ? ESCAPE '\' OR label LIKE ? ESCAPE '\' OR invite_code LIKE ? ESCAPE '\' OR folder LIKE ? ESCAPE '\')`)
		args = append(args, like, like, like, like)
	}
	if f.From != nil {
		where = append(where, "submitted_at >= ?")
		args = append(args, ts(*f.From))
	}
	if f.To != nil {
		where = append(where, "submitted_at < ?")
		args = append(args, ts(*f.To))
	}
	w := strings.Join(where, " AND ")
	var total int
	if err := s.db.QueryRow(`SELECT count(*) FROM submissions WHERE `+w, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if f.Limit <= 0 {
		f.Limit = 50
	}
	rows, err := s.db.Query(`SELECT `+submissionCols+` FROM submissions WHERE `+w+` ORDER BY submitted_at DESC, id LIMIT ? OFFSET ?`,
		append(args, f.Limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []Submission
	for rows.Next() {
		sub, err := scanSubmission(rows, false)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *sub)
	}
	return out, total, rows.Err()
}

func (s *Store) DeleteSubmission(id string) error {
	res, err := s.db.Exec(`DELETE FROM submissions WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) FolderExists(folder string) bool {
	var n int
	s.db.QueryRow(`SELECT count(*) FROM submissions WHERE folder = ?`, folder).Scan(&n)
	return n > 0
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// Action log.

const (
	ActorAdmin  = "admin"
	ActorGuest  = "guest"
	ActorSystem = "system"
)

type Entry struct {
	ID        int64
	At        time.Time
	ActorKind string
	Actor     string
	IP        string
	Action    string
	Detail    string
}

func (s *Store) Log(kind, actor, ip, action, detail string) error {
	_, err := s.db.Exec(`INSERT INTO audit(at, actor_kind, actor, ip, action, detail) VALUES (?, ?, ?, ?, ?, ?)`,
		now(), kind, actor, ip, action, detail)
	return err
}

type LogFilter struct {
	From      *time.Time
	To        *time.Time
	ActorKind string
	Actor     string // substring
	Action    string // exact
	Text      string // substring of detail
	Limit     int
	Offset    int
}

func (s *Store) LogEntries(f LogFilter) ([]Entry, int, error) {
	where := []string{"1=1"}
	var args []any
	if f.From != nil {
		where = append(where, "at >= ?")
		args = append(args, ts(*f.From))
	}
	if f.To != nil {
		where = append(where, "at < ?")
		args = append(args, ts(*f.To))
	}
	if f.ActorKind != "" {
		where = append(where, "actor_kind = ?")
		args = append(args, f.ActorKind)
	}
	if a := strings.TrimSpace(f.Actor); a != "" {
		where = append(where, `actor LIKE ? ESCAPE '\'`)
		args = append(args, "%"+escapeLike(a)+"%")
	}
	if f.Action != "" {
		where = append(where, "action = ?")
		args = append(args, f.Action)
	}
	if t := strings.TrimSpace(f.Text); t != "" {
		where = append(where, `(detail LIKE ? ESCAPE '\' OR ip LIKE ? ESCAPE '\')`)
		like := "%" + escapeLike(t) + "%"
		args = append(args, like, like)
	}
	w := strings.Join(where, " AND ")
	var total int
	if err := s.db.QueryRow(`SELECT count(*) FROM audit WHERE `+w, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if f.Limit <= 0 {
		f.Limit = 100
	}
	rows, err := s.db.Query(`SELECT id, at, actor_kind, actor, ip, action, detail FROM audit WHERE `+w+` ORDER BY id DESC LIMIT ? OFFSET ?`,
		append(args, f.Limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		var at string
		if err := rows.Scan(&e.ID, &at, &e.ActorKind, &e.Actor, &e.IP, &e.Action, &e.Detail); err != nil {
			return nil, 0, err
		}
		e.At = parseTS(at)
		out = append(out, e)
	}
	return out, total, rows.Err()
}

// LogActions lists the distinct action names for the filter dropdown.
func (s *Store) LogActions() ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT action FROM audit ORDER BY action`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		rows.Scan(&a)
		out = append(out, a)
	}
	return out, rows.Err()
}
