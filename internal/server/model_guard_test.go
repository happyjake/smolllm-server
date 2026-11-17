package server

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The requested model string becomes labels in the ledger, the breaker and
// the /metrics output; hostile names are rejected at the door so the
// downstream caps only ever see sane strings.
func TestHostileModelNamesAreRejectedAtTheDoor(t *testing.T) {
	t.Parallel()

	router := newBreakerRouter(t, map[string]string{"bot": "fake/b"}, false)
	for name, model := range map[string]string{
		"overlong": strings.Repeat("m", maxRequestModelLen+1),
		"nul":      "bo\x00t",
		"control":  "bo\tt",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			payload := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, model)
			req, err := http.NewRequest(http.MethodPost, router.URL+"/v1/chat/completions", strings.NewReader(payload))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer local-test")
			resp, err := router.Client().Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		})
	}
}

// The embeddings endpoint shares the door.
func TestHostileModelNamesAreRejectedAtTheEmbeddingsDoor(t *testing.T) {
	t.Parallel()

	router := newBreakerRouter(t, map[string]string{"bot": "fake/b"}, false)
	payload := fmt.Sprintf(`{"model":%q,"input":"hi"}`, strings.Repeat("m", maxRequestModelLen+1))
	req, err := http.NewRequest(http.MethodPost, router.URL+"/v1/embeddings", strings.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer local-test")
	resp, err := router.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestValidateModelNameTable(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		model string
		ok    bool
	}{
		{"plain", "doom/hy4flash", true},
		{"chinese", "混元/极速", true},
		{"c1 control", "bo\u0085t", false}, // NEL, hidden from a byte scan
		{"bidi override", "bo\u202et", false},
		{"tab", "bo\tt", false},
		{"nul", "bo\x00t", false},
		{"overlong", strings.Repeat("m", maxRequestModelLen+1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.ok {
				assert.NoError(t, validateModelName(tc.model))
			} else {
				assert.Error(t, validateModelName(tc.model))
			}
		})
	}
}
