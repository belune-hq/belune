package worker

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"

	"github.com/weiliang79/belune/internal/store/generated"
)

// The health check's port: the app's own wins, then the first domain that
// declares one, then 8080. A zero is "unset" at every step, matching
// proxy.ResolveUpstreamPort's guard — a stored 0 used to be probed as :0.
func TestResolveContainerPort(t *testing.T) {
	port := func(v int32) pgtype.Int4 { return pgtype.Int4{Int32: v, Valid: true} }
	dom := func(v pgtype.Int4) generated.Domain { return generated.Domain{ContainerPort: v} }

	for name, tc := range map[string]struct {
		app     pgtype.Int4
		domains []generated.Domain
		want    int32
	}{
		"app port wins over domains":    {port(3000), []generated.Domain{dom(port(3001))}, 3000},
		"first declaring domain":        {pgtype.Int4{}, []generated.Domain{dom(pgtype.Int4{}), dom(port(3001))}, 3001},
		"nothing set":                   {pgtype.Int4{}, nil, 8080},
		"zero app port is unset":        {port(0), []generated.Domain{dom(port(3001))}, 3001},
		"zero domain port is skipped":   {pgtype.Int4{}, []generated.Domain{dom(port(0)), dom(port(3001))}, 3001},
		"only a zero domain port falls": {pgtype.Int4{}, []generated.Domain{dom(port(0))}, 8080},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, resolveContainerPort(generated.Application{ContainerPort: tc.app}, tc.domains))
		})
	}
}
