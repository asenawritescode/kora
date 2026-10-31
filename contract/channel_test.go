package contract

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestValidateRevokeManagedChannelClientV2(t *testing.T) {
	fingerprint := sha256.Sum256([]byte("managed-token"))
	valid := RevokeManagedChannelClientV2{CredentialFingerprint: hex.EncodeToString(fingerprint[:])}
	if err := ValidateRevokeManagedChannelClientV2(valid); err != nil {
		t.Fatalf("valid fingerprint rejected: %v", err)
	}
	for _, value := range []string{"", "deadbeef", string(make([]byte, 64)), "zz" + valid.CredentialFingerprint[2:]} {
		if err := ValidateRevokeManagedChannelClientV2(RevokeManagedChannelClientV2{CredentialFingerprint: value}); err == nil {
			t.Errorf("invalid fingerprint %q accepted", value)
		}
	}
}
