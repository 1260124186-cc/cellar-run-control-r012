package workflowcheck

import (
	"fmt"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/service"
)

// requireAggregateStates reads both sides independently and verifies they agree
// with the expected lifecycle state.
func requireAggregateStates(
	api *client,
	runID string,
	vesselID string,
	runState domain.RunState,
	vesselState domain.VesselState,
	phase string,
) error {
	runResponse, err := api.call("GET", "/v1/runs/"+runID, nil, nil)
	if err != nil {
		return err
	}
	if err := requireStatus(runResponse, 200, "read run "+phase); err != nil {
		return err
	}
	var run runView
	if err := runResponse.decode(&run); err != nil {
		return err
	}
	if err := requireRunState(run, runState); err != nil {
		return fmt.Errorf("%s: %w", phase, err)
	}

	vesselResponse, err := api.call("GET", "/v1/vessels/"+vesselID, nil, nil)
	if err != nil {
		return err
	}
	if err := requireStatus(vesselResponse, 200, "read vessel "+phase); err != nil {
		return err
	}
	var vessel vesselView
	if err := vesselResponse.decode(&vessel); err != nil {
		return err
	}
	if err := requireVesselState(vessel, vesselState); err != nil {
		return fmt.Errorf("%s: %w", phase, err)
	}
	return nil
}

// verifyCleanupRecovery re-reads every aggregate from the reopened store and
// checks that list and detail responses match the last successful writes.
func verifyCleanupRecovery(app *service.Service) error {
	runs, err := app.ListRuns()
	if err != nil {
		return err
	}
	vessels, err := app.ListVessels()
	if err != nil {
		return err
	}
	if len(runs) == 0 || len(vessels) == 0 {
		return errInvalidWorkflow("recovered snapshot is missing aggregates")
	}

	byCode := map[string]domain.FermentationRun{}
	for _, run := range runs {
		byCode[run.Code] = run
		detail, err := app.GetRun(run.ID)
		if err != nil {
			return err
		}
		if detail.State != run.State || detail.Version != run.Version {
			return errInvalidWorkflow("run detail disagrees with run list after restart")
		}
	}

	byCodeVessel := map[string]domain.Vessel{}
	for _, vessel := range vessels {
		byCodeVessel[vessel.Code] = vessel
		detail, err := app.GetVessel(vessel.ID)
		if err != nil {
			return err
		}
		if detail.State != vessel.State || detail.Version != vessel.Version {
			return errInvalidWorkflow("vessel detail disagrees with vessel list after restart")
		}
	}

	// Pre-start abort: vessel released, run keeps a historical link only.
	releasedAbort := byCode["RUN-CLN-ABORT-RESERVED"]
	if releasedAbort.State != domain.RunAborted {
		return errInvalidWorkflow("recovered pre-start abort run is not aborted")
	}
	vessel1 := byCodeVessel["VSL-CLN-1"]
	if vessel1.State != domain.VesselAvailable || vessel1.ActiveRunID != nil {
		return errInvalidWorkflow("recovered vessel from pre-start abort is not released")
	}
	if releasedAbort.VesselID == nil || *releasedAbort.VesselID != vessel1.ID {
		return errInvalidWorkflow("recovered aborted run lost its vessel history")
	}

	// Post-fermentation abort and completion both end with the vessel released
	// after cleaning was confirmed.
	for _, code := range []string{"RUN-CLN-ABORT-FERMENTING", "RUN-CLN-COMPLETE"} {
		run := byCode[code]
		if run.State != domain.RunAborted && run.State != domain.RunCompleted {
			return errInvalidWorkflow("recovered finished run has unexpected state: " + code)
		}
		var codeVessel domain.Vessel
		if code == "RUN-CLN-ABORT-FERMENTING" {
			codeVessel = byCodeVessel["VSL-CLN-2"]
		} else {
			codeVessel = byCodeVessel["VSL-CLN-3"]
		}
		if codeVessel.State != domain.VesselAvailable || codeVessel.ActiveRunID != nil {
			return errInvalidWorkflow("recovered vessel was not released after cleaning: " + code)
		}
		if run.VesselID == nil || *run.VesselID != codeVessel.ID {
			return errInvalidWorkflow("recovered finished run lost its vessel link: " + code)
		}
	}
	return nil
}

// verifyLifecycleRecovery confirms the fermenting run and in-use vessel survive
// a restart with both sides of the association intact.
func verifyLifecycleRecovery(app *service.Service) error {
	runs, err := app.ListRuns()
	if err != nil {
		return err
	}
	var found *domain.FermentationRun
	for index := range runs {
		if runs[index].Code == "RUN-LIFECYCLE-A" {
			found = &runs[index]
		}
	}
	if found == nil {
		return errInvalidWorkflow("lifecycle run missing after restart")
	}
	if found.State != domain.RunFermenting || found.VesselID == nil {
		return errInvalidWorkflow("lifecycle run state did not survive restart")
	}
	vessel, err := app.GetVessel(*found.VesselID)
	if err != nil {
		return err
	}
	if vessel.State != domain.VesselInUse ||
		vessel.ActiveRunID == nil || *vessel.ActiveRunID != found.ID {
		return errInvalidWorkflow("lifecycle vessel association did not survive restart")
	}
	return nil
}

// verifyCompletionRecovery confirms a completed run and its cleaning vessel are
// both recovered with the run association intact.
func verifyCompletionRecovery(app *service.Service) error {
	runs, err := app.ListRuns()
	if err != nil {
		return err
	}
	var found *domain.FermentationRun
	for index := range runs {
		if runs[index].Code == "RUN-OBSERVATIONS" {
			found = &runs[index]
		}
	}
	if found == nil {
		return errInvalidWorkflow("completed run missing after restart")
	}
	if found.State != domain.RunCompleted || found.VesselID == nil || found.Metrics == nil {
		return errInvalidWorkflow("completed run did not survive restart")
	}
	vessel, err := app.GetVessel(*found.VesselID)
	if err != nil {
		return err
	}
	if vessel.State != domain.VesselCleaning {
		return errInvalidWorkflow("completed run's vessel is not cleaning after restart")
	}
	if vessel.ActiveRunID == nil || *vessel.ActiveRunID != found.ID {
		return errInvalidWorkflow("cleaning vessel lost its run association after restart")
	}
	return nil
}
