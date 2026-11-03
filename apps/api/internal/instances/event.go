package instances

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/store/generated"
)

// EventQuerier is the slice of generated.Queries ResolveForContainer reads.
type EventQuerier interface {
	GetInstanceByContainerID(ctx context.Context, arg generated.GetInstanceByContainerIDParams) (generated.ApplicationInstance, error)
	GetInstanceByContainerName(ctx context.Context, arg generated.GetInstanceByContainerNameParams) (generated.ApplicationInstance, error)
	GetInstanceByProcessTypeAndIdx(ctx context.Context, arg generated.GetInstanceByProcessTypeAndIdxParams) (generated.ApplicationInstance, error)
}

// ResolveForContainer finds the instance row a running container belongs to,
// given what a Docker event carries. Three keys, in this order:
//
//  1. container_id -- the most precise, once a deploy has recorded it.
//  2. container_name against the STORED container_name. This is what identifies
//     every container created before the labels existed: A1's backfill stored the
//     exact name those containers already have. It is a lookup of a stored fact,
//     not a parse of the name.
//  3. the application-id + process-type + instance-idx labels -- what new
//     containers carry, and what several instances of one application will need.
//
// ⛔ Never derive the instance by splitting the name apart.
//
// found=false with a nil error means no row matched; callers treat that as "not
// ours to record" rather than as a failure.
func ResolveForContainer(ctx context.Context, q EventQuerier, appID pgtype.UUID, ev runtime.ContainerEvent) (inst generated.ApplicationInstance, found bool, err error) {
	if ev.ContainerID != "" {
		inst, err = q.GetInstanceByContainerID(ctx, generated.GetInstanceByContainerIDParams{
			ApplicationID: appID, ContainerID: pgtype.Text{String: ev.ContainerID, Valid: true},
		})
		if err == nil {
			return inst, true, nil
		}
		if !isNoRows(err) {
			return inst, false, fmt.Errorf("resolving instance by container id: %w", err)
		}
	}
	if ev.ContainerName != "" {
		inst, err = q.GetInstanceByContainerName(ctx, generated.GetInstanceByContainerNameParams{
			ApplicationID: appID, ContainerName: ev.ContainerName,
		})
		if err == nil {
			return inst, true, nil
		}
		if !isNoRows(err) {
			return inst, false, fmt.Errorf("resolving instance by container name: %w", err)
		}
	}
	processType := ev.Labels[runtime.LabelProcessType]
	if idx, convErr := strconv.Atoi(ev.Labels[runtime.LabelInstanceIdx]); processType != "" && convErr == nil {
		inst, err = q.GetInstanceByProcessTypeAndIdx(ctx, generated.GetInstanceByProcessTypeAndIdxParams{
			ApplicationID: appID, Name: processType, Idx: int32(idx),
		})
		if err == nil {
			return inst, true, nil
		}
		if !isNoRows(err) {
			return inst, false, fmt.Errorf("resolving instance by labels: %w", err)
		}
	}
	return generated.ApplicationInstance{}, false, nil
}

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
