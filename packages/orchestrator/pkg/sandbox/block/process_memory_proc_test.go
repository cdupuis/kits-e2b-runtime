//go:build linux

package block

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

func TestProcessMemoryProcPackedRanges(t *testing.T) {
	t.Parallel()

	const page = int64(header.PageSize)
	addr, data := allocateTestMemory(t, uint64(3*page), uint64(page))
	cache, err := NewCache(2*page, page, t.TempDir()+"/cache", false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, cache.Close()) })

	require.NoError(t, cache.copyProcessMemoryFromProc(t.Context(), os.Getpid(), []Range{
		{Start: int64(addr) + 2*page, Size: page},
		{Start: int64(addr), Size: page},
	}))
	actual := make([]byte, 2*page)
	n, err := cache.ReadAt(actual, 0)
	require.NoError(t, err)
	require.Len(t, actual, n)
	require.Equal(t, data[2*page:3*page], actual[:page])
	require.Equal(t, data[:page], actual[page:])
}

func TestProcessMemoryProcErrors(t *testing.T) {
	t.Parallel()

	const page = int64(header.PageSize)
	for _, tc := range []struct {
		name     string
		pid      int
		canceled bool
	}{
		{name: "unmapped address", pid: os.Getpid()},
		{name: "missing process", pid: -1},
		{name: "canceled", pid: os.Getpid(), canceled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cache, err := NewCache(page, page, t.TempDir()+"/cache", false)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, cache.Close()) })
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.canceled {
				cancel()
			}
			err = cache.copyProcessMemoryFromProc(ctx, tc.pid, []Range{{Start: 0, Size: page}})
			require.Error(t, err)
			if tc.canceled {
				require.ErrorIs(t, err, context.Canceled)
			}
			buf := make([]byte, page)
			_, err = cache.ReadAt(buf, 0)
			require.ErrorIs(t, err, BytesNotAvailableError{})
		})
	}
}
