package workflowcheck

import (
	"fmt"
	"net/http/httptest"
	"os"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/clock"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/httpapi"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/service"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/storage"
)

func Run(name string) error {
	dataDir, err := os.MkdirTemp("", "cellar-run-check-*")
	if err != nil {
		return fmt.Errorf("create check directory: %w", err)
	}
	defer os.RemoveAll(dataDir)

	store, err := storage.Open(dataDir)
	if err != nil {
		return err
	}
	app := service.New(store, clock.System{})
	server := httptest.NewServer(httpapi.NewHandler(app))
	api := newClient(server.URL)

	switch name {
	case "formula-approval":
		err = formulaApproval(api)
	case "vessel-run-lifecycle":
		err = vesselRunLifecycle(api)
	case "observations-completion":
		err = observationsCompletion(api)
	case "run-cleanup":
		err = runCleanup(api, dataDir)
	default:
		server.Close()
		return fmt.Errorf("unknown workflow check %q", name)
	}
	if err != nil {
		server.Close()
		return err
	}
	server.Close()

	// Reopen the persisted snapshot the way the real service does on restart.
	// List and detail reads after restart must match the last successful writes.
	reopened, err := storage.Open(dataDir)
	if err != nil {
		return fmt.Errorf("reopen persisted state: %w", err)
	}
	recovered := service.New(reopened, clock.System{})
	switch name {
	case "vessel-run-lifecycle":
		err = verifyLifecycleRecovery(recovered)
	case "observations-completion":
		err = verifyCompletionRecovery(recovered)
	case "run-cleanup":
		err = verifyCleanupRecovery(recovered)
	}
	if err != nil {
		return err
	}
	fmt.Printf("workflow check passed: %s\n", name)
	return nil
}
