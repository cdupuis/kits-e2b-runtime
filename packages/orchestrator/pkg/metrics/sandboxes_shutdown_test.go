//go:build linux

package metrics

import (
	"testing"

	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
)

func TestSandboxObserverCloseStopsItsReader(t *testing.T) {
	t.Parallel()

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(t.Context()) })
	observer := &SandboxObserver{meterProvider: provider}
	var result metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &result))
	require.NoError(t, observer.Close(t.Context()))
	require.ErrorIs(t, reader.Collect(t.Context(), &result), sdkmetric.ErrReaderShutdown)
}

func TestSandboxObserverWithoutCollectorDoesNotStartExporter(t *testing.T) {
	t.Parallel()
	if telemetry.OTELCollectorGRPCEndpoint() != "" {
		t.Skip("requires the ordinary telemetry-disabled local configuration")
	}
	observer, err := NewSandboxObserver(t.Context(), "test", "test", "", "", "", sandbox.NewSandboxesMap())
	require.NoError(t, err)
	require.Nil(t, observer.meterProvider)
	require.Nil(t, observer.registration)
	require.NoError(t, observer.Close(t.Context()))
}
