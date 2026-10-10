package papertrade

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/erickuhn19/deliverator/internal/state"
)

// StoredIntent is the exact POST body of a signed intent, kept so an exit 40
// or 42 can resend that body instead of signing a new nonce.
type StoredIntent struct {
	IntentID  string          `json:"intent_id"`
	Action    string          `json:"action"`
	Path      string          `json:"path"`
	Body      json.RawMessage `json:"body"`
	Deadline  string          `json:"deadline"`
	User      string          `json:"user"`
	CreatedMs int64           `json:"created_ms"`
	State     string          `json:"state,omitempty"`
}

// IntentStore writes one file per intent under dir. Cancel records use a
// "cancel-" prefix so they do not overwrite the open they cancel.
type IntentStore struct{ dir string }

func NewIntentStore(dir string) *IntentStore { return &IntentStore{dir: dir} }

// intentOnDisk stores the POST body as a JSON string. MarshalIndent would
// otherwise rewrap a json.RawMessage, and a resend must post the same bytes
// that were signed.
type intentOnDisk struct {
	IntentID  string `json:"intent_id"`
	Action    string `json:"action"`
	Path      string `json:"path"`
	Body      string `json:"body"`
	Deadline  string `json:"deadline"`
	User      string `json:"user"`
	CreatedMs int64  `json:"created_ms"`
	State     string `json:"state,omitempty"`
}

func (s *IntentStore) Put(in StoredIntent) error {
	if s == nil || s.dir == "" {
		return errString("intent store is not configured")
	}
	name, err := intentFile(in.Action, in.IntentID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(intentOnDisk{
		IntentID: in.IntentID, Action: in.Action, Path: in.Path, Body: string(in.Body),
		Deadline: in.Deadline, User: in.User, CreatedMs: in.CreatedMs, State: in.State,
	}, "", "  ")
	if err != nil {
		return err
	}
	return state.WriteFileAtomic(filepath.Join(s.dir, name), append(b, '\n'), 0o600)
}

func (s *IntentStore) Get(action, id string) (StoredIntent, error) {
	name, err := intentFile(action, id)
	if err != nil {
		return StoredIntent{}, err
	}
	b, err := os.ReadFile(filepath.Join(s.dir, name))
	if err != nil {
		return StoredIntent{}, err
	}
	var rec intentOnDisk
	if err := json.Unmarshal(b, &rec); err != nil {
		return StoredIntent{}, err
	}
	return StoredIntent{
		IntentID: rec.IntentID, Action: rec.Action, Path: rec.Path,
		Body: json.RawMessage(rec.Body), Deadline: rec.Deadline, User: rec.User,
		CreatedMs: rec.CreatedMs, State: rec.State,
	}, nil
}

// Find looks up an open/close/register record, then a cancel record.
func (s *IntentStore) Find(id string) (StoredIntent, error) {
	if in, err := s.Get("", id); err == nil {
		return in, nil
	}
	return s.Get("cancelIntent", id)
}

func intentFile(action, id string) (string, error) {
	id = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(id)), "0x")
	if len(id) != 64 || !hexOnly(id) {
		return "", errString("intent id must be 32 bytes of hex")
	}
	name := id + ".json"
	if action == "cancelIntent" {
		name = "cancel-" + name
	}
	return name, nil
}

func hexOnly(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}
