//go:build linux

package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"time"

	"connectrpc.com/connect"
	"github.com/docker/kits-e2b/guest"
	"github.com/google/uuid"

	"github.com/e2b-dev/infra/packages/shared/pkg/consts"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/envd/process"
)

// RunKitRequest uses envd's built-in executor; no uploaded executable or SDK
// login shell is involved. A PTY workload survives the Start stream closing
// and is subsequently addressable through Process.Connect's kit-workload tag.
//
//nolint:contextcheck // kitWorkloadContext inherits cancellation while intentionally hiding the create deadline.
func (s *Sandbox) RunKitRequest(ctx context.Context, r guest.Request, workload bool) error {
	// Node uses bundled roots by default. Add the envd-managed public CA bundle
	// to workload environments when this node supplies egress trust. Hooks
	// already have their declared environment allowlist applied; adding a name
	// here would bypass that boundary.
	r = kitRequestWithTrust(r, workload, s.egressProxy != nil && s.egressProxy.CABundle(s.ExpectedEnvdVersion()) != "")
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if len(raw) > 4<<20 {
		return errors.New("kit request exceeds 4 MiB")
	}
	// This directory is on /run, so requests never enter shared templates and
	// disappear on cold boot. The only interpolated name is a generated UUID.
	if err := s.runKitSetup(ctx); err != nil {
		return err
	}
	name := "/run/e2b-kit/" + uuid.NewString() + ".json"
	base := fmt.Sprintf("http://%s:%d", s.Slot.HostIPString(), consts.DefaultEnvdServerPort)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/files?username=root&path="+url.QueryEscape(name), bytes.NewReader(raw))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/octet-stream")
	request.SetBasicAuth("root", "")
	if s.Config.Envd.AccessToken != nil {
		request.Header.Set("X-Access-Token", *s.Config.Envd.AccessToken)
	}
	response, err := NewEnvdHTTPClient(time.Minute).Do(request)
	if err != nil {
		return err
	}
	_, drainErr := io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	closeErr := response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("stage Kit request: HTTP %d", response.StatusCode)
	}
	if drainErr != nil {
		return drainErr
	}
	if closeErr != nil {
		return closeErr
	}
	req := connect.NewRequest(&process.StartRequest{Process: &process.ProcessConfig{Cmd: "/usr/bin/envd", Args: []string{"kit-execute", name}}, Stdin: new(workload)})
	if workload {
		req.Msg.Tag = new("kit-workload")
		req.Msg.Pty = &process.PTY{Size: &process.PTY_Size{Cols: 80, Rows: 24}}
	} else {
		req.Header().Set("Connect-Timeout-Ms", "300000")
	}
	if s.Config.Envd.AccessToken != nil {
		req.Header().Set("X-Access-Token", *s.Config.Envd.AccessToken)
	}
	grpc.SetUserHeader(req.Header(), "root")
	if workload {
		// Connect serializes a context deadline into Connect-Timeout-Ms,
		// which envd also uses as the process lifetime. Keep create cancellation
		// for the Start handshake without imposing that deadline on the workload.
		ctx = kitWorkloadContext{ctx}
	}
	stream, err := NewEnvdProcessClient(base, 0).Start(ctx, req)
	if err != nil {
		return err
	}
	defer stream.Close()
	for stream.Receive() {
		event := stream.Msg().GetEvent()
		if workload && event.GetStart() != nil {
			return nil
		}
		if end := event.GetEnd(); end != nil {
			if !end.GetExited() || end.GetExitCode() != 0 {
				return fmt.Errorf("kit %s failed (exit %d)", r.Action, end.GetExitCode())
			}

			return nil
		}
	}
	if err := stream.Err(); err != nil {
		return err
	}

	return errors.New("kit process stream ended without outcome")
}

func kitRequestWithTrust(r guest.Request, workload, hasTrust bool) guest.Request {
	if workload && hasTrust && r.Env != nil {
		r.Env = maps.Clone(r.Env)
		r.Env["NODE_EXTRA_CA_CERTS"] = "/etc/ssl/certs/ca-certificates.crt"
	}

	return r
}

//nolint:containedctx // Wrap the parent only to override deadline serialization; cancellation remains inherited.
type kitWorkloadContext struct{ context.Context }

func (kitWorkloadContext) Deadline() (time.Time, bool) { return time.Time{}, false }

func (s *Sandbox) runKitSetup(ctx context.Context) error {
	stream, err := s.StartEnvdShell(ctx, "/bin/sh", []string{"-c", "umask 077; test ! -L /run/e2b-kit && mkdir -p /run/e2b-kit && chmod 700 /run/e2b-kit"}, "root", time.Minute)
	if err != nil {
		return err
	}
	defer stream.Close()
	for stream.Receive() {
		if end := stream.Msg().GetEvent().GetEnd(); end != nil {
			if !end.GetExited() || end.GetExitCode() != 0 {
				return errors.New("prepare private Kit request directory failed")
			}

			return nil
		}
	}
	if err := stream.Err(); err != nil {
		return err
	}

	return errors.New("prepare Kit request directory ended without outcome")
}
