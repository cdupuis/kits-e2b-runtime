//go:build linux

// Package kit executes host-compiled Kit requests in an envd child process.
package kit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strconv"
	"syscall"

	"github.com/docker/kits-e2b/guest"

	"github.com/e2b-dev/infra/packages/envd/internal/reexec"
)

const (
	Directory       = "/run/e2b-kit"
	MaxRequestBytes = 4 << 20
)

var Command = reexec.Define("kit-execute", run)

func readRequest(name string) (guest.Request, error) {
	var r guest.Request
	if filepath.Dir(name) != Directory || filepath.Clean(name) != name {
		return r, errors.New("kit request outside private directory")
	}
	fd, err := syscall.Open(name, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return r, err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return r, err
	}
	if !info.Mode().IsRegular() || info.Size() > MaxRequestBytes {
		return r, errors.New("invalid Kit request file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, MaxRequestBytes+1))
	if err != nil {
		return r, err
	}
	if len(raw) > MaxRequestBytes {
		return r, errors.New("kit request too large")
	}
	if err = json.Unmarshal(raw, &r); err != nil {
		return r, errors.New("invalid Kit request")
	}
	if err = os.Remove(name); err != nil {
		return r, err
	}

	return r, nil
}

func checkIdentity(id guest.Identity) error {
	passwd, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return err
	}
	group, err := os.ReadFile("/etc/group")
	if err != nil {
		return err
	}
	actual, err := guest.ResolveIdentity(string(passwd), string(group), strconv.FormatUint(uint64(id.UID), 10)+":"+strconv.FormatUint(uint64(id.GID), 10))
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(id, actual) {
		return errors.New("kit identity changed after image composition")
	}

	return nil
}

func run(args []string) error {
	if len(args) != 1 {
		return errors.New("expected Kit request path")
	}
	r, err := readRequest(args[0])
	if err != nil {
		return err
	}
	if err = checkIdentity(r.Identity); err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	switch r.Action {
	case "tmpfs":
		return guest.MountTmpfs(r.Tmpfs, r.Identity)
	case "terminal":
		return guest.ExecTerminal(r)
	case "exec":
		code, err := guest.Execute(ctx, r, os.Stdout, os.Stderr)
		if err != nil {
			return err
		}
		if code != 0 {
			return commandExitError(code)
		}

		return nil
	case "write", "skills":
		if r.Action == "write" && r.File == nil {
			return errors.New("missing Kit file")
		}
		groups := make([]int, len(r.Identity.Groups))
		for i, g := range r.Identity.Groups {
			groups[i] = int(g)
		}
		if err = syscall.Setgroups(groups); err != nil {
			return err
		}
		if err = syscall.Setgid(int(r.Identity.GID)); err != nil {
			return err
		}
		if err = syscall.Setuid(int(r.Identity.UID)); err != nil {
			return err
		}

		if r.Action == "skills" {
			if r.Skills == nil {
				return errors.New("missing Kit skills")
			}

			return guest.ExposeSkills(*r.Skills)
		}

		return guest.WriteFile(*r.File)
	default:
		return errors.New("unknown Kit action")
	}
}

type commandExitError int

func (e commandExitError) Error() string { return fmt.Sprintf("kit command exited with status %d", e) }
func (e commandExitError) ExitCode() int { return int(e) }
