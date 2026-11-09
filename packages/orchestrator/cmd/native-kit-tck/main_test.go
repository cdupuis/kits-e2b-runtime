package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/docker/sandbox-kit-spec/v3/spec"
	"github.com/docker/sandbox-kit-spec/v3/tck/report"
	"github.com/stretchr/testify/require"
)

func TestTCKReportRetainsInterruptedObservations(t *testing.T) {
	t.Parallel()
	var rep report.Report
	rep.Add("observed-pass", "requirement")
	var complete bytes.Buffer
	code, err := writeTCKReport(&complete, rep, nil)
	require.NoError(t, err)
	require.Zero(t, code)
	var result map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(complete.Bytes(), &result))
	require.NotContains(t, result, "incomplete")
	require.NotContains(t, result, "error")

	rep.Add("interrupted-check", "requirement", report.Failf("probe canceled"))
	var partial bytes.Buffer
	code, err = writeTCKReport(&partial, rep, context.DeadlineExceeded)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, 1, code)
	require.NoError(t, json.Unmarshal(partial.Bytes(), &result))
	require.JSONEq(t, `true`, string(result["incomplete"]))
	require.JSONEq(t, `"context deadline exceeded"`, string(result["error"]))
	var retained report.Report
	require.NoError(t, json.Unmarshal(result["report"], &retained))
	require.Equal(t, rep, retained)

	var failed bytes.Buffer
	code, err = writeTCKReport(&failed, rep, nil)
	require.NoError(t, err)
	require.Equal(t, 1, code)
	result = nil
	require.NoError(t, json.Unmarshal(failed.Bytes(), &result))
	require.NotContains(t, result, "incomplete")
}

func TestAdmissionRefusalKeepsInfrastructureFailures(t *testing.T) {
	t.Parallel()
	_, invalid := spec.Validate(&spec.Descriptor{Kind: "invalid"})
	require.Error(t, invalid)
	for _, err := range []error{
		invalid,
		fmt.Errorf("fixture: %w", invalid),
		errors.New("fixture: required capability selection rejected (item capabilities[0])"),
		errors.New("fixture: capabilities[0].group.capabilities[0]: invalid capability configuration after environment expansion"),
		errors.New("resolve: the kit set is not coherent:\n  - conflict"),
		errors.New("merge: first and second declare different com.docker.sandbox/volume@1 configurations at /data"),
		errors.New("merge: first and second declare different com.docker.sandbox/host-mount@1 configurations at /shared"),
		errors.New("merge: first and second both declare storage at the same path; a host mount has one owner"),
		errors.New("merge: first and second: /path is written by two contributions; one file has one author"),
		errors.New("merge: first and second both declare a lifecycle interactive tail; it replaces the launch command's arguments, so it has one author"),
		errors.New("agent-context: selected @1 and @2 requests cannot compose; migrate all selected guidance to one version"),
	} {
		require.True(t, admissionRefusal(err), "%v", err)
	}
	for _, err := range []error{
		nil,
		os.ErrPermission,
		errors.New("no space left on device"),
		errors.New("registry: HTTP 500"),
		errors.Join(errors.New("merge: first and second declare different com.docker.sandbox/volume@1 configurations at /data"), os.ErrPermission),
		errors.Join(errors.New("merge: first and second both declare storage at the same path; a host mount has one owner"), os.ErrPermission),
		errors.New("hook exited 1"),
		errors.Join(invalid, os.ErrPermission),
		fmt.Errorf("wrapped: %w", errors.Join(invalid, os.ErrPermission)),
	} {
		require.False(t, admissionRefusal(err), "%v", err)
	}
}
