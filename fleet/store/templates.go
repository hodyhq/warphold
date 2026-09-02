package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kopia/kopia/snapshot/policy"
)

type Template struct {
	ID         int64
	Name       string
	Sources    []string
	PolicyJSON json.RawMessage
	CreatedAt  time.Time
}

const templateCols = `id,name,sources,policy_json,created_at`

// ErrBadPolicyJSON is returned when a template's PolicyJSON is not a Kopia policy object.
var ErrBadPolicyJSON = errors.New("policy_json must be a Kopia policy object")

// normalizePolicyJSON mirrors the API layer's validation (templateIn.validate)
// at the store boundary, so no caller can persist a policy_json that the list
// endpoint would later stream out as malformed JSON: empty means "no
// overrides", anything else must unmarshal into a policy object.
func normalizePolicyJSON(raw json.RawMessage) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return json.RawMessage(`{}`), nil
	}

	if !bytes.HasPrefix(trimmed, []byte("{")) {
		return nil, ErrBadPolicyJSON
	}

	var p policy.Policy
	if err := json.Unmarshal(trimmed, &p); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadPolicyJSON, err)
	}

	return trimmed, nil
}

func (s *Store) CreateTemplate(ctx context.Context, t *Template) (int64, error) {
	src, err := json.Marshal(t.Sources)
	if err != nil {
		return 0, err
	}
	pol, err := normalizePolicyJSON(t.PolicyJSON)
	if err != nil {
		return 0, err
	}
	return s.exec(ctx, `INSERT INTO policy_templates(name,sources,policy_json,created_at) VALUES(?,?,?,?)`, t.Name, string(src), string(pol), ts(t.CreatedAt))
}

func (s *Store) UpdateTemplate(ctx context.Context, t *Template) error {
	src, err := json.Marshal(t.Sources)
	if err != nil {
		return err
	}
	pol, err := normalizePolicyJSON(t.PolicyJSON)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE policy_templates SET name=?,sources=?,policy_json=? WHERE id=?`, t.Name, string(src), string(pol), t.ID)
	return err
}

func scanTemplate(row interface{ Scan(...any) error }) (*Template, error) {
	var t Template
	var src, pol, c string
	if err := row.Scan(&t.ID, &t.Name, &src, &pol, &c); err != nil {
		return nil, notFound(err)
	}
	_ = json.Unmarshal([]byte(src), &t.Sources)
	t.PolicyJSON = json.RawMessage(pol)
	t.CreatedAt = parseTS(c)
	return &t, nil
}

func (s *Store) Template(ctx context.Context, id int64) (*Template, error) {
	return scanTemplate(s.db.QueryRowContext(ctx, `SELECT `+templateCols+` FROM policy_templates WHERE id=?`, id))
}

func (s *Store) Templates(ctx context.Context) ([]Template, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+templateCols+` FROM policy_templates ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Template
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}
