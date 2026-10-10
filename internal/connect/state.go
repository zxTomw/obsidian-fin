package connect

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"obsidian-fin/internal/domain"
	"obsidian-fin/internal/fsutil"
)

type FileState struct{ Dir string }

func (s FileState) UserID() (string, error) {
	path := filepath.Join(s.Dir, "client-user-id")
	b, err := os.ReadFile(path)
	if err == nil {
		id := strings.TrimSpace(string(b))
		if !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(id) {
			return "", errors.New("invalid saved user identifier")
		}
		return id, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	id, err := fsutil.ID()
	if err != nil {
		return "", err
	}
	if err := fsutil.AtomicPrivate(path, []byte(id+"\n")); err != nil {
		return "", err
	}
	return id, nil
}
func (s FileState) Recovery() (*domain.Recovery, error) {
	path := filepath.Join(s.Dir, "connection-recovery.json")
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r domain.Recovery
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	if r.TokenRef == "" || r.ClientUserID == "" {
		return nil, errors.New("invalid recovery reference")
	}
	return &r, nil
}
func (s FileState) SaveRecovery(r domain.Recovery) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return fsutil.AtomicPrivate(filepath.Join(s.Dir, "connection-recovery.json"), append(b, '\n'))
}
func (s FileState) ClearRecovery() error {
	err := os.Remove(filepath.Join(s.Dir, "connection-recovery.json"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return fsutil.SyncDir(s.Dir)
}
