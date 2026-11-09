// Package kitcredentials implements the opt-in, host-only Embed credential backend.
// It is deliberately independent of the managed E2B secrets service.
package kitcredentials

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/net/http/httpguts"
	"golang.org/x/sys/unix"

	"github.com/e2b-dev/infra/packages/shared/pkg/secretsstore"
)

var errUnavailable = errors.New("native Kit secret unavailable")

// Resolver receives only a host-authenticated project UUID and a declared name.
// Implementations must not include values in errors or logs.
type Resolver interface {
	Resolve(ctx context.Context, project, name string) (string, error)
}

// FileStore reads an administrator-owned file on every request. Atomic replacement
// rotates or removes credentials without restarting workers or existing TLS sessions.
type FileStore struct{ path string }

type document struct {
	Projects map[string]map[string]string `json:"projects"`
}

func NewFileStore(path string) (*FileStore, error) {
	s := &FileStore{path: path}
	if _, err := s.read(); err != nil {
		return nil, err
	}

	return s, nil
}

// ReadPrivateFile refuses links, special files and access by other users. Parent
// directories and the configured path are host-administrator authority.
func ReadPrivateFile(path string) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, errUnavailable
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, errUnavailable
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	var stat unix.Stat_t
	if unix.Fstat(fd, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0o077 != 0 || stat.Uid != uint32(os.Geteuid()) {
		return nil, errUnavailable
	}
	b, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(b) > 1<<20 {
		return nil, errUnavailable
	}

	return b, nil
}

func (s *FileStore) read() (*document, error) {
	b, err := ReadPrivateFile(s.path)
	if err != nil {
		return nil, err
	}
	var d document
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&d) != nil || decoder.Decode(new(any)) != io.EOF || d.Projects == nil {
		return nil, errUnavailable
	}
	for project, secrets := range d.Projects {
		id, err := uuid.Parse(project)
		if err != nil || id == uuid.Nil || id.String() != project {
			return nil, errUnavailable
		}
		for name, value := range secrets {
			normalized, err := secretsstore.NormalizeName(name)
			if err != nil || normalized != name || value == "" || len(value) > 64<<10 || !httpguts.ValidHeaderFieldValue(value) {
				return nil, errUnavailable
			}
		}
	}

	return &d, nil
}

func (s *FileStore) Resolve(ctx context.Context, project, name string) (string, error) {
	if ctx.Err() != nil {
		return "", errUnavailable
	}
	d, err := s.read()
	if err != nil {
		return "", err
	}
	value := d.Projects[project][name]
	if value == "" {
		return "", errUnavailable
	}

	return value, nil
}

// Expand accepts exactly one named marker. Names are normalized like the E2B
// management API; arbitrary interpolation and recursive expansion are forbidden.
func Expand(ctx context.Context, resolver Resolver, project, format string) (string, error) {
	const prefix = "${e2b.secrets."
	before, rest, ok := strings.Cut(format, prefix)
	name, after, closed := strings.Cut(rest, "}")
	if !ok || !closed || strings.Contains(before+after, "${") {
		return "", errUnavailable
	}
	name, err := secretsstore.NormalizeName(name)
	if err != nil {
		return "", errUnavailable
	}
	value, err := resolver.Resolve(ctx, project, name)
	if err != nil {
		return "", errUnavailable
	}
	result := before + value + after
	if value == "" || len(result) > 64<<10 || !httpguts.ValidHeaderFieldValue(result) {
		return "", errUnavailable
	}

	return result, nil
}
