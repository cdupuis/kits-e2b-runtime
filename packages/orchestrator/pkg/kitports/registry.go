package kitports

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

const Environment = "NATIVE_KIT_PORTS_DIR"

func privateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return errors.New("native port registry must be a private directory")
	}

	return nil
}

func ValidateDirectory(root string) error {
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root || root == "/" {
		return errors.New("native ports need an absolute dedicated registry directory")
	}

	return privateDirectory(root)
}

type Record struct {
	Team      string     `json:"team"`
	Instance  string     `json:"instance"`
	Lifecycle string     `json:"lifecycle"`
	Endpoints []Endpoint `json:"endpoints"`
}
type registration struct {
	file *os.File
	path string
}

var instanceID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func register(root, team, instance, lifecycle string, endpoints []Endpoint) (_ *registration, retErr error) {
	if _, err := uuid.Parse(team); err != nil {
		return nil, err
	}
	if _, err := uuid.Parse(lifecycle); err != nil {
		return nil, err
	}
	if !instanceID.MatchString(instance) {
		return nil, errors.New("invalid native port instance")
	}
	if err := ValidateDirectory(root); err != nil {
		return nil, err
	}
	dir := filepath.Join(root, team)
	if err := privateDirectory(dir); err != nil {
		return nil, err
	}
	dir = filepath.Join(dir, instance)
	if err := privateDirectory(dir); err != nil {
		return nil, err
	}
	file, err := os.CreateTemp(dir, ".ports-*")
	if err != nil {
		return nil, err
	}
	temp := file.Name()
	defer func() {
		_ = os.Remove(temp)
		if retErr != nil {
			_ = file.Close()
		}
	}()
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, err
	}
	if err := json.NewEncoder(file).Encode(Record{Team: team, Instance: instance, Lifecycle: lifecycle, Endpoints: endpoints}); err != nil {
		return nil, err
	}
	if err := file.Sync(); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, lifecycle+".json")
	// Lifecycle UUIDs are unique; never replace an existing live registration.
	if err := os.Link(temp, path); err != nil {
		return nil, fmt.Errorf("publish native port record: %w", err)
	}

	return &registration{file: file, path: path}, nil
}

func (r *registration) close() error {
	err := os.Remove(r.path)
	if os.IsNotExist(err) {
		err = nil
	}

	return errors.Join(err, r.file.Close())
}
