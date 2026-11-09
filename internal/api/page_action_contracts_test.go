package api

import (
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
)

func TestServerPageActionRegistryMatchesEngineContracts(t *testing.T) {
	runtimes := map[string]bool{}
	for name := range serverPageActionRegistry {
		runtimes[name] = true
		contract, ok := doctype.PageActionContractFor(name)
		if !ok {
			t.Errorf("server action %q has no Engine validation contract", name)
			continue
		}
		if contract.Execution != "server" {
			t.Errorf("server action %q contract execution = %q", name, contract.Execution)
		}
	}
	for _, contract := range doctype.PageActionContracts() {
		if contract.Execution == "server" && !runtimes[contract.Name] {
			t.Errorf("server contract %q has no registered handler", contract.Name)
		}
	}
}
