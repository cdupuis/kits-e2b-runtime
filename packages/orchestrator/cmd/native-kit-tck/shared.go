package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"

	kitstorage "github.com/docker/kits-e2b/storage"
)

func sharedStorage(ctx context.Context, args []string) (int, error) {
	want := 3
	if args[0] == "host-mount-read" {
		want = 4
	}
	if len(args) != want {
		return 1, errors.New("shared storage needs team, Kit reference or handle, and optional relative path")
	}
	store, err := kitstorage.Open(os.Getenv(kitstorage.Environment))
	if err != nil {
		return 1, err
	}
	switch args[0] {
	case "host-mounts":
		mounts, err := store.Shared(ctx, args[1], args[2])
		if err != nil {
			return 1, err
		}
		type hostMount struct {
			ID       string `json:"id"`
			Path     string `json:"path"`
			HostPath string `json:"hostPath"`
		}
		result := make([]hostMount, 0, len(mounts))
		for _, m := range mounts {
			result = append(result, hostMount{ID: m.ID, Path: m.Volume.Path, HostPath: m.Directory})
		}

		return 0, json.NewEncoder(os.Stdout).Encode(result)
	case "host-mount-read":
		data, err := store.ReadShared(ctx, args[1], args[2], args[3])
		if err != nil {
			return 1, err
		}
		_, err = os.Stdout.Write(data)

		return 0, err
	case "host-mount-rm":
		return 0, store.RemoveShared(ctx, args[1], args[2])
	default:
		return 1, errors.New("unknown shared storage operation")
	}
}
