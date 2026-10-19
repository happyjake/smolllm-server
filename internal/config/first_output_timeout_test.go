package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeConfig(t *testing.T, body string) *Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	cfg, err := Load(path)
	require.NoError(t, err)
	return cfg
}

func TestFirstOutputTimeout_OmittedInheritsLibraryDefault(t *testing.T) {
	t.Parallel()
	cfg := writeConfig(t, `
server:
  access_key: k
aliases:
  agent: doom/gpt,zai/glm
`)
	// Nil, not zero: the caller must be able to tell "operator said nothing"
	// (inherit the safe default) from "operator disabled it" (0).
	require.Nil(t, cfg.Server.FirstOutputTimeout)
}

func TestFirstOutputTimeout_ExplicitZeroDisables(t *testing.T) {
	t.Parallel()
	cfg := writeConfig(t, `
server:
  access_key: k
  first_output_timeout: 0
aliases:
  agent: doom/gpt,zai/glm
`)
	require.NotNil(t, cfg.Server.FirstOutputTimeout)
	require.Equal(t, 0.0, *cfg.Server.FirstOutputTimeout)
}

func TestFirstOutputTimeout_NegativeRejected(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
server:
  access_key: k
  first_output_timeout: -5
`), 0o600))
	_, err := Load(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), "first_output_timeout")
}

// The guard is only worth anything if the whole chain's worst case still
// finishes before the client stops waiting. A 5-model chain at 120s each is
// worse than the incident it was meant to prevent, so loading it must fail
// loudly instead of quietly shipping a 600s budget against a 300s client.
func TestFirstOutputBudget_RejectsChainLongerThanClientPatience(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
server:
  access_key: k
  first_output_timeout: 120
aliases:
  agent: doom/gpt,zai/glm,kimi/k3,astra/k3,taiji/hy4
`), 0o600))
	_, err := Load(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), "client header timeout")
	require.Contains(t, err.Error(), "agent")
}

func TestFirstOutputBudget_AcceptsChainThatFits(t *testing.T) {
	t.Parallel()
	cfg := writeConfig(t, `
server:
  access_key: k
  first_output_timeout: 30
aliases:
  agent: doom/gpt,zai/glm,kimi/k3,astra/k3,taiji/hy4
`)
	// 5 x 30 = 150s, inside the 300s default.
	require.Equal(t, 30.0, *cfg.Server.FirstOutputTimeout)
}

func TestFirstOutputBudget_HonoursConfiguredClientPatience(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
server:
  access_key: k
  first_output_timeout: 30
  client_header_timeout: 60
aliases:
  agent: doom/gpt,zai/glm,kimi/k3
`), 0o600))
	// 3 x 30 = 90s against a 60s client: the configured limit must be used
	// rather than the default, so a host serving impatient clients is caught.
	_, err := Load(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), "60s")
}

// A disabled guard cannot exceed anything, so validation must not reject
// aliases for it - otherwise turning the feature off would require shrinking
// chains first.
func TestFirstOutputBudget_DisabledSkipsValidation(t *testing.T) {
	t.Parallel()
	cfg := writeConfig(t, `
server:
  access_key: k
  first_output_timeout: 0
aliases:
  long: a/m1,b/m2,c/m3,d/m4,e/m5,f/m6,g/m7,h/m8,i/m9,j/m10,k/m11,l/m12
`)
	require.Equal(t, 0.0, *cfg.Server.FirstOutputTimeout)
	require.Len(t, cfg.Aliases, 1)
}
