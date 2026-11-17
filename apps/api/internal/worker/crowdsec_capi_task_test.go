package worker

import (
	"context"
	"testing"

	"github.com/belune-hq/belune/internal/crowdsec"
	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/require"
)

func TestCrowdSecCAPITaskRejectsUnusablePayloadsWithoutApplying(t *testing.T) {
	missing := &TaskHandler{}
	require.ErrorIs(t, missing.HandleCrowdSecCAPITask(context.Background(), asynq.NewTask(TypeCrowdSecCAPI, []byte(`{"enabled":true}`))), asynq.SkipRetry)
	// Nil dependencies make an accidental Apply call fail immediately: malformed
	// and empty messages must never reach any Docker operation.
	handler := &TaskHandler{CAPI: crowdsec.NewCAPIManager(nil, nil)}
	for _, payload := range []string{`not-json`, `{}`, `{"enabled":"yes"}`} {
		require.ErrorIs(t, handler.HandleCrowdSecCAPITask(context.Background(), asynq.NewTask(TypeCrowdSecCAPI, []byte(payload))), asynq.SkipRetry)
	}
}
