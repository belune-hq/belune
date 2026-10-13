package service_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/weiliang79/belune/internal/service"
)

func TestValidateContainerPort(t *testing.T) {
	for _, ok := range []int{1, 80, 3000, 65535} {
		p, err := service.ValidateContainerPort(ok)
		require.NoError(t, err)
		assert.True(t, p.Valid)
		assert.EqualValues(t, ok, p.Int32)
	}
	// For an application, 0 is an explicit clear.
	p, err := service.ValidateContainerPort(0)
	require.NoError(t, err)
	assert.False(t, p.Valid)
	for _, bad := range []int{-1, 65536, 99999} {
		_, err := service.ValidateContainerPort(bad)
		assert.Error(t, err, "%d", bad)
	}
}

func TestValidateDomainContainerPort(t *testing.T) {
	i32 := func(v int32) *int32 { return &v }

	// Absent and 0 both mean "no override" and store NULL, never a Valid zero.
	for _, unset := range []*int32{nil, i32(0)} {
		p, err := service.ValidateDomainContainerPort(unset)
		require.NoError(t, err)
		assert.False(t, p.Valid)
	}
	p, err := service.ValidateDomainContainerPort(i32(3001))
	require.NoError(t, err)
	assert.True(t, p.Valid)
	assert.EqualValues(t, 3001, p.Int32)

	// The range rule is the application's, not a second copy.
	for _, bad := range []int32{-1, 65536, 99999} {
		_, err := service.ValidateDomainContainerPort(i32(bad))
		assert.Error(t, err, "%d", bad)
	}
}
