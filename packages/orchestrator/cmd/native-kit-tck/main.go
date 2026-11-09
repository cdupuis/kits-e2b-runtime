// Native Kit TCK utilities use the same worker admission and envd Process API as
// the runtime. They neither emulate guest behavior nor rewrite exec arguments.
package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"connectrpc.com/connect"
	kitstorage "github.com/docker/kits-e2b/storage"
	"github.com/docker/sandbox-kit-spec/v3/spec"
	"github.com/docker/sandbox-kit-spec/v3/tck/adapter"
	"github.com/docker/sandbox-kit-spec/v3/tck/report"
	"github.com/docker/sandbox-kit-spec/v3/tck/sandbox"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/daemon"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/kit"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/envd/process"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/envd/process/processconnect"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Hour)
	code, err := run(ctx, os.Args[1:])
	cancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func run(ctx context.Context, args []string) (int, error) {
	if len(args) == 0 {
		return 1, errors.New("expected plan, exec, publish, registry or run")
	}
	switch args[0] {
	case "types":
		return 0, json.NewEncoder(os.Stdout).Encode(spec.KnownCapabilities())
	case "fixtures":
		if len(args) != 2 {
			return 1, errors.New("fixtures needs an output directory")
		}
		source, cleanup, err := sandbox.MaterializeFixtures()
		if err != nil {
			return 1, err
		}
		defer cleanup()

		return 0, os.CopyFS(args[1], os.DirFS(source))
	case "describe":
		if len(args) != 2 {
			return 1, errors.New("describe needs a fixture descriptor")
		}
		raw, err := os.ReadFile(args[1])
		if err != nil {
			return 1, err
		}
		d, err := spec.Decode(raw)
		if err != nil {
			return 1, err
		}

		return 0, json.NewEncoder(os.Stdout).Encode(d)
	case "registry":
		if len(args) != 2 {
			return 1, errors.New("registry needs a loopback listen address")
		}

		return 1, http.ListenAndServe(args[1], registry.New(registry.Logger(log.New(os.Stderr, "registry ", log.LstdFlags))))
	case "plan":
		raw, err := io.ReadAll(io.LimitReader(os.Stdin, (1<<20)+1))
		if err != nil {
			return 1, err
		}
		p, err := kit.Prepare(ctx, raw, runtime.GOARCH, false, kit.Backends{Storage: os.Getenv("NATIVE_TCK_STORAGE") == "1", Ports: os.Getenv("NATIVE_TCK_PORTS") == "1"})
		if err != nil {
			// This is the selector's explicit policy refusal. Registry, syntax,
			// integrity and I/O errors stay errors, never successful refusal probes.
			if admissionRefusal(err) {
				return 2, err
			}

			return 1, err
		}
		defer p.Close()
		execution, err := p.Bundle.ExecutionPlan()
		if err != nil {
			return 1, err
		}
		resources, err := p.Bundle.Plan.ResourceRequirements()
		if err != nil {
			return 1, err
		}
		selected, skipped := []spec.SelectionRecord{}, []spec.SelectionRecord{}
		for _, entry := range p.Bundle.Plan.Resolved.Selections {
			selected = append(selected, entry.Selection.Selected...)
			skipped = append(skipped, entry.Selection.Skipped...)
		}

		requests, err := p.Bundle.StorageRequests()
		if err != nil {
			return 1, err
		}

		return 0, json.NewEncoder(os.Stdout).Encode(map[string]any{"storage": requests, "identity": execution.Workload.Identity, "env": execution.Workload.Env, "cwd": execution.Workload.Cwd, "resources": resources, "selection": map[string]any{"surface": spec.SurfaceOf(p.Bundle.Plan.Resolved.Descriptor), "selection": map[string]any{"selected": selected, "skipped": skipped}}})
	case "host-mounts", "host-mount-read", "host-mount-rm":
		return sharedStorage(ctx, args)
	case "volume-paths", "storage-check":
		if len(args) != 3 {
			return 1, errors.New("storage observation needs team and sandbox ID")
		}
		store, err := kitstorage.Open(os.Getenv(kitstorage.Environment))
		if err != nil {
			return 1, err
		}
		if args[0] == "volume-paths" {
			paths, err := store.Paths(ctx, args[1], args[2])
			if err != nil {
				return 1, err
			}

			return 0, json.NewEncoder(os.Stdout).Encode(paths)
		}
		var requests []kitstorage.Request
		if err := json.NewDecoder(io.LimitReader(os.Stdin, 1<<20)).Decode(&requests); err != nil {
			return 1, err
		}
		err = store.Check(ctx, args[1], args[2], requests)
		if errors.Is(err, kitstorage.ErrIncompatible) {
			return 2, err
		}

		return 0, err

	case "exec", "disconnect":
		if len(args) < 2 || (args[0] == "exec" && len(args) < 3) {
			return 1, errors.New("exec needs state file and argv")
		}
		raw, err := os.ReadFile(args[1])
		if err != nil {
			return 1, err
		}
		var state struct {
			SandboxID       string            `json:"sandboxID"`
			EnvdAccessToken string            `json:"envdAccessToken"`
			User            string            `json:"user"`
			Cwd             string            `json:"cwd"`
			Env             map[string]string `json:"env"`
		}
		if err = json.Unmarshal(raw, &state); err != nil {
			return 1, err
		}
		command := "true"
		var commandArgs []string
		if args[0] == "exec" {
			command, commandArgs = args[2], args[3:]
		}
		config := &process.ProcessConfig{Cmd: command, Args: commandArgs, Cwd: &state.Cwd, Envs: state.Env}
		req := connect.NewRequest(&process.StartRequest{Process: config, Stdin: new(false)})
		grpc.SetUserHeader(req.Header(), state.User)
		req.Header().Set("X-Access-Token", state.EnvdAccessToken)
		req.Header().Set("E2b-Sandbox-Id", state.SandboxID)
		req.Header().Set("E2b-Sandbox-Port", "49983")
		base := os.Getenv("NATIVE_TCK_ENVD_URL")
		if base == "" {
			base = "http://127.0.0.1:5007"
		}
		client := processconnect.NewProcessClient(&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, base)
		if args[0] == "disconnect" {
			connection := connect.NewRequest(&process.ConnectRequest{Process: &process.ProcessSelector{Selector: &process.ProcessSelector_Tag{Tag: "kit-workload"}}})
			connection.Header().Set("Authorization", req.Header().Get("Authorization"))
			connection.Header().Set("X-Access-Token", state.EnvdAccessToken)
			connection.Header().Set("E2b-Sandbox-Id", state.SandboxID)
			connection.Header().Set("E2b-Sandbox-Port", "49983")
			stream, err := client.Connect(ctx, connection)
			if err != nil {
				return 1, err
			}
			defer stream.Close()
			if !stream.Receive() {
				if err := stream.Err(); err != nil {
					return 1, err
				}

				return 1, errors.New("attachment ended before an event")
			}

			return 0, nil
		}
		stream, err := client.Start(ctx, req)
		if err != nil {
			return 1, err
		}
		defer stream.Close()
		for stream.Receive() {
			event := stream.Msg().GetEvent()
			if d := event.GetData(); d != nil {
				if _, err = os.Stdout.Write(d.GetStdout()); err != nil {
					return 1, err
				}
				if _, err = os.Stderr.Write(d.GetStderr()); err != nil {
					return 1, err
				}
			}
			if end := event.GetEnd(); end != nil {
				if !end.GetExited() {
					return 1, errors.New("process did not exit")
				}

				return int(end.GetExitCode()), nil
			}
		}
		if err = stream.Err(); err != nil {
			return 1, err
		}

		return 1, errors.New("process stream ended without exit event")
	case "publish":
		if len(args) != 4 {
			return 1, errors.New("publish needs fixture directory, Docker image (or - for mixin), target reference")
		}

		return 0, publish(ctx, args[1], args[2], args[3])
	case "run":
		if len(args) != 3 {
			return 1, errors.New("run needs adapter executable and fixture catalog JSON")
		}
		raw, err := os.ReadFile(args[2])
		if err != nil {
			return 1, err
		}
		catalog := map[string]string{}
		if err = json.Unmarshal(raw, &catalog); err != nil {
			return 1, err
		}
		a := adapter.New(args[1])
		rep, err := sandbox.Run(ctx, &sandbox.Env{Adapter: a, Fixtures: func(n string) string {
			if ref, ok := catalog[n]; ok {
				return ref
			}

			return "missing-fixture:" + n
		}})

		return writeTCKReport(os.Stdout, rep, err)
	default:
		return 1, errors.New("unknown verb")
	}
}

// Keep partial observations without presenting an interrupted run as conformance.
func writeTCKReport(w io.Writer, rep report.Report, runErr error) (int, error) {
	result := map[string]any{"summary": rep.Summary(), "report": rep}
	if runErr != nil {
		result["incomplete"] = true
		result["error"] = runErr.Error()
	}
	if err := json.NewEncoder(w).Encode(result); err != nil {
		return 1, errors.Join(runErr, err)
	}
	if runErr != nil {
		return 1, runErr
	}
	if rep.Summary().Failed > 0 {
		return 1, nil
	}

	return 0, nil
}

// Refusal is reserved for the compiler's declaration/selection diagnostics.
// Inspect joined errors separately so an I/O cleanup failure cannot be hidden
// by a simultaneous validation error. The pinned spec scrubs environment-
// expanded validation errors to an untyped message to avoid exposing values.
func admissionRefusal(err error) bool {
	if err == nil {
		return false
	}
	var field *spec.FieldError
	if errors.As(err, &field) && err == field { //nolint:errorlint // Match only the outer error; a wrapped join can also contain I/O failures.
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !admissionRefusal(cause) {
				return false
			}
		}

		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return admissionRefusal(wrapped.Unwrap())
	}
	message := err.Error()

	return strings.Contains(message, "required capability selection rejected") ||
		strings.HasSuffix(message, ": invalid capability configuration after environment expansion") ||
		strings.HasPrefix(message, "resolve: the kit set is not coherent:\n") ||
		message == "agent-context: selected @1 and @2 requests cannot compose; migrate all selected guidance to one version" ||
		(strings.HasPrefix(message, "merge: ") && (strings.HasSuffix(message, "one file has one author") ||
			strings.HasSuffix(message, "so it has one author") ||
			strings.HasSuffix(message, " both declare storage at the same path; a host mount has one owner") ||
			strings.Contains(message, " declare different "+spec.CapabilityVolume+" configurations at ") ||
			strings.Contains(message, " declare different "+spec.CapabilityHostMount+" configurations at ")))
}

func publish(ctx context.Context, dir, image, target string) error {
	raw, err := os.ReadFile(filepath.Join(dir, filepath.Base(dir)+".yaml"))
	if err != nil {
		return err
	}
	d, err := spec.Decode(raw)
	if err != nil {
		return err
	}
	if _, err = spec.ValidateRaw(raw, d); err != nil {
		return err
	}
	values, err := spec.ResolveArgs(d.Args, nil)
	if err != nil {
		return err
	}
	raw, err = spec.ExpandBuildArgs(raw, d.Args, values)
	if err != nil {
		return err
	}
	d, err = spec.Decode(raw)
	if err != nil {
		return err
	}
	img := empty.Image
	if image != "-" {
		tag, err := name.NewTag(image)
		if err != nil {
			return err
		}
		img, err = daemon.Image(tag, daemon.WithContext(ctx))
		if err != nil {
			return err
		}
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		return err
	}
	cfg.OS = "linux"
	cfg.Architecture = runtime.GOARCH
	img, err = mutate.ConfigFile(img, cfg)
	if err != nil {
		return err
	}
	prefix := "/usr/share/sandbox/kit/" + filepath.Base(dir)
	// Normalize authored contentFile paths into this fixture's staged source
	// root. Fixture sources are retained verbatim under a unique Kit directory.
	var rewrite func([]spec.Capability)
	rewrite = func(items []spec.Capability) {
		for i := range items {
			c := &items[i]
			if c.Group != nil {
				rewrite(c.Group.Capabilities)

				continue
			}
			if s, ok := c.Config["contentFile"].(string); ok && !strings.HasPrefix(s, "/") {
				c.Config["contentFile"] = prefix + "/" + strings.TrimPrefix(s, "./")
			}
		}
	}
	rewrite(d.Capabilities)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	err = filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("fixture contains non-regular file: %s", path)
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		h, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		h.Name = strings.TrimPrefix(prefix, "/") + "/" + filepath.ToSlash(rel)
		if err = tw.WriteHeader(h); err != nil {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		_, err = tw.Write(b)

		return err
	})
	if err != nil {
		return err
	}
	if err = tw.Close(); err != nil {
		return err
	}
	layer, err := tarball.LayerFromReader(&buf)
	if err != nil {
		return err
	}
	img, err = mutate.AppendLayers(img, layer)
	if err != nil {
		return err
	}
	raw, err = json.Marshal(d)
	if err != nil {
		return err
	}
	if _, err = spec.ValidatePublished(raw, d); err != nil {
		return err
	}
	img = mutate.Annotations(img, map[string]string{spec.AnnotationDescriptor: string(raw), spec.AnnotationSchemaVersion: "3"}).(v1.Image)
	dst, err := name.NewTag(target, name.Insecure)
	if err != nil {
		return err
	}

	return remote.Write(dst, img, remote.WithContext(ctx))
}
