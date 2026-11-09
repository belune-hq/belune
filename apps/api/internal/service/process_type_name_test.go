package service

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInstanceName_CarriesTheGenerationSuffix(t *testing.T) {
	g := newGeneration()
	assert.Regexp(t, regexp.MustCompile(`^[0-9a-f]{8}$`), g)
	n, err := instanceName("proj-app-5586176d", "worker", 2, g)
	require.NoError(t, err)
	assert.Equal(t, "proj-app-5586176d-worker-2-"+g, n)
	assert.NotEqual(t, g, newGeneration(), "two generations must not share a token")
}

// The suffix makes a name 9 characters longer; the limit has to count it.
func TestInstanceName_TheLimitCountsTheSuffix(t *testing.T) {
	slug := strings.Repeat("a", dockerNameLimit-len("-w-1")-1) // fits without a suffix, not with
	_, err := instanceName(slug, "w", 1, "0123abcd")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "255")
}
