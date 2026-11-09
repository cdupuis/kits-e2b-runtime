// Package kit admits Kit v3 artifacts before the template builder boots a VM.
package kit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	e2b "github.com/docker/kits-e2b"
	"github.com/docker/sandbox-kit-spec/v3/fetch"
	"github.com/docker/sandbox-kit-spec/v3/spec"
	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry/remote/auth"
)

// Source contains declarations and secret names, never secret values. Registry
// credentials are supplied separately; daemon Docker credentials are not read.
type Source struct {
	References []string           `json:"references"`
	Options    e2b.PrepareOptions `json:"options,omitzero"`
	Args       map[string]string  `json:"args,omitempty"`
	Env        map[string]string  `json:"env,omitempty"`
}

type Prepared struct {
	Bundle    *e2b.Bundle
	Image     v1.Image
	Key       string
	directory string
}

func (p *Prepared) Close() error { return os.RemoveAll(p.directory) }

// Backends are trusted worker features; source JSON cannot enable them.
type Backends struct {
	Storage bool
	Ports   bool
}

func Prepare(ctx context.Context, raw []byte, architecture string, credentials bool, nativeBackends ...Backends) (_ *Prepared, retErr error) {
	source, err := DecodeSource(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid Kit source: %w", err)
	}
	source.Options.NativeStorage = len(nativeBackends) > 0 && nativeBackends[0].Storage
	source.Options.NativePorts = len(nativeBackends) > 0 && nativeBackends[0].Ports
	loader, err := e2b.RegistryLoaderForPlatform(ocispec.Platform{OS: "linux", Architecture: architecture}, func(context.Context, string) (auth.Credential, error) { return auth.EmptyCredential, nil })
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "e2b-kit-")
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			_ = os.RemoveAll(dir)
		}
	}()
	requests := make([]fetch.Request, len(source.References))
	knownArgs := make(map[string]bool)
	for i, ref := range source.References {
		requests[i] = fetch.Request{Reference: ref}
		if len(source.Args) == 0 {
			continue
		}
		published, err := loader(ctx, ref)
		if err != nil {
			return nil, err
		}
		raw := published.Descriptor
		if len(raw) == 0 {
			raw = []byte(published.Manifest.Annotations[spec.AnnotationDescriptor])
		}
		descriptor, err := spec.Decode(raw)
		if err != nil {
			return nil, err
		}
		requests[i].Args = make(map[string]string)
		for name := range descriptor.Args {
			knownArgs[name] = true
			if value, ok := source.Args[name]; ok {
				requests[i].Args[name] = value
			}
		}
	}
	for name := range source.Args {
		if !knownArgs[name] {
			return nil, fmt.Errorf("unknown Kit argument %s", name)
		}
	}
	bundleDir := filepath.Join(dir, "bundle")
	plan, err := e2b.PrepareWithEnvironment(ctx, bundleDir, requests, loader, nil, source.Options, source.Env)
	if err != nil {
		return nil, err
	}
	// The receipt is an adapter validation envelope, not a cloud allocation.
	size, err := plan.ResourceRequirements()
	if err != nil {
		return nil, err
	}
	receipt := filepath.Join(dir, "receipt.json")
	if err = e2b.WriteJSON(receipt, e2b.ImportReceipt{Resources: size, TemplateID: "native", BuildID: "native", Image: "native.invalid/kit@" + plan.Image.Digest.String()}); err != nil {
		return nil, err
	}
	bundle, err := e2b.LoadBundle(ctx, bundleDir, receipt)
	if err != nil {
		return nil, err
	}
	if _, err = Admit(bundle, credentials); err != nil {
		return nil, err
	}
	store, err := layout.FromPath(filepath.Join(bundleDir, "oci"))
	if err != nil {
		return nil, err
	}
	hash, err := v1.NewHash(plan.Image.Digest.String())
	if err != nil {
		return nil, err
	}
	image, err := store.Image(hash)
	if err != nil {
		return nil, err
	}
	// Hash resolved content and instance declarations: tags, policy overrides,
	// guidance and arguments must not alias an existing base-layer cache entry.
	encoded, err := json.Marshal(bundle)
	if err != nil {
		return nil, err
	}
	key := fmt.Sprintf("kit-v3:%x", sha256.Sum256(encoded))

	return &Prepared{Bundle: bundle, Image: image, Key: key, directory: dir}, nil
}

// DecodeSource rejects misspelled options rather than silently losing policy.
func DecodeSource(raw []byte) (Source, error) {
	var source Source
	if len(raw) > 1<<20 {
		return source, errors.New("kit source exceeds 1 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&source); err != nil {
		return source, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return source, errors.New("trailing Kit source data")
	}
	if len(source.References) < 1 || len(source.References) > 32 {
		return source, errors.New("kit source requires 1–32 references")
	}
	for _, ref := range source.References {
		if strings.TrimSpace(ref) != ref || ref == "" {
			return source, errors.New("empty or whitespace-padded Kit reference")
		}
	}

	return source, nil
}

// Admit compiles only contracts the configured backend can enforce. Without
// an explicit credential injector, credential-bearing templates fail closed.
func Admit(bundle *e2b.Bundle, credentials bool) (*e2b.ExecutionPlan, error) {
	plan, err := bundle.ExecutionPlan()
	if err != nil {
		return nil, err
	}
	if !credentials && (len(plan.Network.Install.Rules) != 0 || len(plan.Network.Runtime.Rules) != 0) {
		return nil, errors.New("native OSS Kit credentials require a configured egress secret-injection backend")
	}

	return plan, nil
}

// SupportsCredentials queries an explicit backend capability, never a client flag.
func SupportsCredentials(proxy any) bool {
	capability, ok := proxy.(interface{ SupportsNativeKitCredentials() bool })

	return ok && capability.SupportsNativeKitCredentials()
}
