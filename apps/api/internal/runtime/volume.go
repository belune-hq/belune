package runtime

import (
	"context"
	"fmt"
)

// RequireVolume prevents Docker's implicit volume creation from turning a
// missing adopted data set into an empty mount or a successful empty backup.
func RequireVolume(ctx context.Context, rt ContainerRuntime, name string) error {
	volumes, err := rt.ListVolumes(ctx)
	if err != nil {
		return fmt.Errorf("check existing volume: %w", err)
	}
	for _, v := range volumes {
		if v.Name == name {
			return nil
		}
	}
	return fmt.Errorf("adopted volume %q is missing; refusing to create an empty replacement", name)
}
