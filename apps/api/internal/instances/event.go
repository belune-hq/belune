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
		// Both generations of a rolling deploy carry the SAME type and idx labels, so
		// this key alone would hand a draining container's late 'die' to the live
		// row. The deployment-id label tells them apart: a container from another
		// deployment than the one the row records is not the row's container. Rows
		// with no recorded deployment (NULL), and events with no label, are matched
		// as before.
		return resolveByLabels(ctx, q, appID, processType, int32(idx), ev.Labels[runtime.LabelDeploymentID])
	}
	return generated.ApplicationInstance{}, false, nil
}

func resolveByLabels(ctx context.Context, q EventQuerier, appID pgtype.UUID, processType string, idx int32, deploymentLabel string) (inst generated.ApplicationInstance, found bool, err error) {
	{
		inst, err = q.GetInstanceByProcessTypeAndIdx(ctx, generated.GetInstanceByProcessTypeAndIdxParams{
			ApplicationID: appID, Name: processType, Idx: idx,
		})
		if err == nil {
			if inst.DeploymentID.Valid && deploymentLabel != "" && uuidString(inst.DeploymentID) != deploymentLabel {
				return generated.ApplicationInstance{}, false, nil
			}
			return inst, true, nil
		}
		if !isNoRows(err) {
			return inst, false, fmt.Errorf("resolving instance by labels: %w", err)
		}
	}
	return generated.ApplicationInstance{}, false, nil
}

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
