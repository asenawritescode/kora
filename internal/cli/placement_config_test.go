package cli

import "testing"

func TestEnginePlacementConfigPrefersDeploymentContractAndAcceptsLegacyAdapter(t *testing.T) {
	localID, endpoints, err := enginePlacementConfig("Deploy-A", "deploy-a", "", "deploy-b=http://127.0.0.1:8081", "")
	if err != nil {
		t.Fatal("deployment config:", err)
	}
	if localID != "deploy-a" || endpoints["deploy-b"] != "http://127.0.0.1:8081" {
		t.Fatalf("deployment placement = %q %#v", localID, endpoints)
	}

	legacyID, legacyEndpoints, err := enginePlacementConfig("", "", "cell-a", "", "cell-b=http://127.0.0.1:8082")
	if err != nil {
		t.Fatal("legacy adapter config:", err)
	}
	if legacyID != "cell-a" || legacyEndpoints["cell-b"] != "http://127.0.0.1:8082" {
		t.Fatalf("legacy placement = %q %#v", legacyID, legacyEndpoints)
	}
	legacyPrecedence, _, err := enginePlacementConfig("", "management-a", "cell-a", "", "")
	if err != nil {
		t.Fatal("legacy placement with management identity:", err)
	}
	if legacyPrecedence != "cell-a" {
		t.Fatalf("legacy placement precedence = %q, want cell-a during compatibility", legacyPrecedence)
	}

	managementID, _, err := enginePlacementConfig("", "managed-a", "", "", "")
	if err != nil {
		t.Fatal("management deployment fallback:", err)
	}
	if managementID != "managed-a" {
		t.Fatalf("management deployment fallback = %q, want managed-a", managementID)
	}
}

func TestEnginePlacementConfigRejectsConflictingAuthorities(t *testing.T) {
	if _, _, err := enginePlacementConfig("deployment-a", "", "cell-a", "", ""); err == nil {
		t.Fatal("conflicting local deployment and legacy cell IDs were accepted")
	}
	if _, _, err := enginePlacementConfig("", "", "", "deployment-b=http://127.0.0.1:8081", ""); err == nil {
		t.Fatal("deployment URLs without local identity were accepted")
	}
	if _, _, err := enginePlacementConfig("deployment-a", "", "", "deployment-b=http://127.0.0.1:8081", "deployment-b=http://127.0.0.1:8082"); err == nil {
		t.Fatal("conflicting deployment and legacy endpoint maps were accepted")
	}
	if _, _, err := enginePlacementConfig("", "", "", "", "deployment-b=http://127.0.0.1:8081"); err == nil {
		t.Fatal("legacy endpoint map without local identity was accepted")
	}
}
