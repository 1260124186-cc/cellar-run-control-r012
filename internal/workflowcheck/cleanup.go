package workflowcheck

import (
	"time"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
)

// runCleanup exercises vessel disposition when a run is wound down:
//
//   - aborting a reserved (not yet fermenting) run releases the vessel at once;
//   - aborting a fermenting run schedules cleaning and keeps the run link;
//   - finishing cleaning requires the linked run to be finished;
//   - completing a run schedules cleaning with the run link preserved, and the
//     vessel is released only after cleaning is confirmed.
func runCleanup(api *client, _ string) error {
	formula, err := ensureApprovedFormula(api, "Cleanup Formula")
	if err != nil {
		return err
	}

	if err := abortReservedRun(api, formula.ID); err != nil {
		return err
	}
	if err := abortFermentingRun(api, formula.ID); err != nil {
		return err
	}
	if err := completeAndReleaseRun(api, formula.ID); err != nil {
		return err
	}
	return nil
}

func abortReservedRun(api *client, formulaID string) error {
	vessel, err := createCheckVessel(api, "VSL-CLN-1", 240)
	if err != nil {
		return err
	}
	run, err := createPlannedRun(api, formulaID, vessel.ID, "RUN-CLN-ABORT-RESERVED")
	if err != nil {
		return err
	}

	reserved, err := api.call("POST", "/v1/runs/"+run.ID+"/reserve",
		map[string]any{"vessel_id": vessel.ID}, ptrInt64(run.Version))
	if err != nil {
		return err
	}
	if err := requireStatus(reserved, 200, "reserve run for pre-start abort"); err != nil {
		return err
	}
	var reservedPair pairView
	if err := reserved.decode(&reservedPair); err != nil {
		return err
	}

	// A stale expected version is rejected and must change neither side.
	stale, err := api.call("POST", "/v1/runs/"+run.ID+"/abort",
		nil, ptrInt64(run.Version))
	if err != nil {
		return err
	}
	if err := requireCode(stale, 412, string(domain.CodeRevisionConflict),
		"abort with stale version"); err != nil {
		return err
	}
	if err := requireAggregateStates(api, run.ID, vessel.ID,
		domain.RunReserved, domain.VesselReserved, "after stale abort"); err != nil {
		return err
	}

	aborted, err := api.call("POST", "/v1/runs/"+run.ID+"/abort",
		nil, ptrInt64(reservedPair.Run.Version))
	if err != nil {
		return err
	}
	if err := requireStatus(aborted, 200, "abort run before fermentation"); err != nil {
		return err
	}
	var pair pairView
	if err := aborted.decode(&pair); err != nil {
		return err
	}
	if err := requireRunState(pair.Run, domain.RunAborted); err != nil {
		return err
	}
	if err := requireVesselState(pair.Vessel, domain.VesselAvailable); err != nil {
		return err
	}
	if pair.Vessel.ActiveRunID != nil {
		return errInvalidWorkflow("released vessel still names an active run")
	}
	if pair.Run.VesselID == nil || *pair.Run.VesselID != vessel.ID {
		return errInvalidWorkflow("aborted run lost its historical vessel link")
	}

	// The released vessel can immediately be reserved by another run.
	replacement, err := createPlannedRun(api, formulaID, vessel.ID, "RUN-CLN-RESERVE-NEXT")
	if err != nil {
		return err
	}
	reReserve, err := api.call("POST", "/v1/runs/"+replacement.ID+"/reserve",
		map[string]any{"vessel_id": vessel.ID}, ptrInt64(replacement.Version))
	if err != nil {
		return err
	}
	if err := requireStatus(reReserve, 200, "re-reserve released vessel"); err != nil {
		return err
	}
	var nextPair pairView
	if err := reReserve.decode(&nextPair); err != nil {
		return err
	}
	// Wind the replacement run down the same way so the vessel ends released.
	nextAbort, err := api.call("POST", "/v1/runs/"+replacement.ID+"/abort",
		nil, ptrInt64(nextPair.Run.Version))
	if err != nil {
		return err
	}
	if err := requireStatus(nextAbort, 200, "abort replacement run"); err != nil {
		return err
	}

	// Aborting the same run twice is rejected without side effects.
	repeat, err := api.call("POST", "/v1/runs/"+run.ID+"/abort",
		nil, ptrInt64(pair.Run.Version))
	if err != nil {
		return err
	}
	if err := requireCode(repeat, 409, string(domain.CodeRunState),
		"abort run twice"); err != nil {
		return err
	}
	return nil
}

func abortFermentingRun(api *client, formulaID string) error {
	vessel, err := createCheckVessel(api, "VSL-CLN-2", 260)
	if err != nil {
		return err
	}
	started, err := createStartedRun(api, formulaID, vessel.ID, "RUN-CLN-ABORT-FERMENTING")
	if err != nil {
		return err
	}

	// Cleaning cannot be finished while the linked run is still open.
	premature, err := api.call("POST", "/v1/vessels/"+vessel.ID+"/finish-cleaning",
		nil, ptrInt64(started.Vessel.Version))
	if err != nil {
		return err
	}
	if err := requireCode(premature, 409, string(domain.CodeRunState),
		"finish cleaning for an open run"); err != nil {
		return err
	}

	aborted, err := api.call("POST", "/v1/runs/"+started.Run.ID+"/abort",
		nil, ptrInt64(started.Run.Version))
	if err != nil {
		return err
	}
	if err := requireStatus(aborted, 200, "abort fermenting run"); err != nil {
		return err
	}
	var pair pairView
	if err := aborted.decode(&pair); err != nil {
		return err
	}
	if err := requireRunState(pair.Run, domain.RunAborted); err != nil {
		return err
	}
	if err := requireVesselState(pair.Vessel, domain.VesselCleaning); err != nil {
		return err
	}
	if pair.Vessel.ActiveRunID == nil || *pair.Vessel.ActiveRunID != started.Run.ID {
		return errInvalidWorkflow("cleaning vessel lost its run association")
	}
	if pair.Run.VesselID == nil || *pair.Run.VesselID != vessel.ID {
		return errInvalidWorkflow("aborted run lost its vessel link")
	}

	// A cleaning vessel cannot be reserved or retired while the run is linked.
	replacement, err := createPlannedRun(api, formulaID, vessel.ID, "RUN-CLN-BLOCKED")
	if err != nil {
		return err
	}
	occupied, err := api.call("POST", "/v1/runs/"+replacement.ID+"/reserve",
		map[string]any{"vessel_id": vessel.ID}, ptrInt64(replacement.Version))
	if err != nil {
		return err
	}
	if err := requireCode(occupied, 409, string(domain.CodeVesselUnavailable),
		"reserve a cleaning vessel"); err != nil {
		return err
	}

	finished, err := api.call("POST", "/v1/vessels/"+vessel.ID+"/finish-cleaning",
		nil, ptrInt64(pair.Vessel.Version))
	if err != nil {
		return err
	}
	if err := requireStatus(finished, 200, "finish cleaning after abort"); err != nil {
		return err
	}
	var clean vesselView
	if err := finished.decode(&clean); err != nil {
		return err
	}
	if err := requireVesselState(clean, domain.VesselAvailable); err != nil {
		return err
	}
	if clean.ActiveRunID != nil {
		return errInvalidWorkflow("released vessel still names a run")
	}

	// Finishing cleaning twice is rejected.
	repeat, err := api.call("POST", "/v1/vessels/"+vessel.ID+"/finish-cleaning",
		nil, ptrInt64(clean.Version))
	if err != nil {
		return err
	}
	if err := requireCode(repeat, 409, string(domain.CodeVesselState),
		"finish cleaning twice"); err != nil {
		return err
	}
	return nil
}

func completeAndReleaseRun(api *client, formulaID string) error {
	vessel, err := createCheckVessel(api, "VSL-CLN-3", 320)
	if err != nil {
		return err
	}
	started, err := createStartedRun(api, formulaID, vessel.ID, "RUN-CLN-COMPLETE")
	if err != nil {
		return err
	}

	base := time.Now().UTC().Add(10 * time.Second)
	version := started.Run.Version
	for sequence := 1; sequence <= 3; sequence++ {
		response, callErr := api.call("POST", "/v1/runs/"+started.Run.ID+"/observations",
			map[string]any{
				"sequence":      sequence,
				"gravity":       1.050 - float64(sequence-1)*0.015,
				"temperature_c": 20.0,
				"observed_at":   verifiedAt(base, sequence),
			}, ptrInt64(version))
		if callErr != nil {
			return callErr
		}
		if err := requireStatus(response, 200, "append cleanup observation"); err != nil {
			return err
		}
		var run runView
		if err := response.decode(&run); err != nil {
			return err
		}
		version = run.Version
	}

	conditioning, err := api.call("POST", "/v1/runs/"+started.Run.ID+"/conditioning",
		nil, ptrInt64(version))
	if err != nil {
		return err
	}
	if err := requireStatus(conditioning, 200, "condition cleanup run"); err != nil {
		return err
	}
	var conditioningRun runView
	if err := conditioning.decode(&conditioningRun); err != nil {
		return err
	}

	completed, err := api.call("POST", "/v1/runs/"+started.Run.ID+"/complete",
		nil, ptrInt64(conditioningRun.Version))
	if err != nil {
		return err
	}
	if err := requireStatus(completed, 200, "complete cleanup run"); err != nil {
		return err
	}
	var pair pairView
	if err := completed.decode(&pair); err != nil {
		return err
	}
	if err := requireRunState(pair.Run, domain.RunCompleted); err != nil {
		return err
	}
	if err := requireVesselState(pair.Vessel, domain.VesselCleaning); err != nil {
		return err
	}
	if pair.Vessel.ActiveRunID == nil || *pair.Vessel.ActiveRunID != started.Run.ID {
		return errInvalidWorkflow("completed run's vessel lost the run association")
	}

	// Detail reads must agree with the write response before release.
	if err := requireAggregateStates(api, started.Run.ID, vessel.ID,
		domain.RunCompleted, domain.VesselCleaning, "after completion"); err != nil {
		return err
	}

	finished, err := api.call("POST", "/v1/vessels/"+vessel.ID+"/finish-cleaning",
		nil, ptrInt64(pair.Vessel.Version))
	if err != nil {
		return err
	}
	if err := requireStatus(finished, 200, "finish cleaning after completion"); err != nil {
		return err
	}
	var clean vesselView
	if err := finished.decode(&clean); err != nil {
		return err
	}
	if err := requireVesselState(clean, domain.VesselAvailable); err != nil {
		return err
	}
	if clean.ActiveRunID != nil {
		return errInvalidWorkflow("vessel retained a run after release")
	}
	return nil
}

func createPlannedRun(
	api *client,
	formulaID string,
	vesselID string,
	code string,
) (runView, error) {
	response, err := api.call("POST", "/v1/runs",
		createRunPayload(code, formulaID), nil)
	if err != nil {
		return runView{}, err
	}
	if err := requireStatus(response, 201, "create planned run"); err != nil {
		return runView{}, err
	}
	var run runView
	if err := response.decode(&run); err != nil {
		return runView{}, err
	}
	return run, nil
}
