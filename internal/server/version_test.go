package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/rocry/smolllm-server/internal/buildinfo"
	"github.com/stretchr/testify/require"
)

func TestVersionRequiresAuthAndReportsRunningProcess(t *testing.T) {
	ts, _ := newTestRig(t, false)
	resp, err := http.Get(ts.URL + "/version")
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp.Body.Close()
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/version", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer rocry")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var info buildinfo.Info
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&info))
	require.Equal(t, buildinfo.Current(), info)
	require.False(t, info.SourceClean, "plain test build is not release provenance")
}
