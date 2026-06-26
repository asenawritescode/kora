package webhook

import (
	"testing"
	"time"
)

func TestSignAndVerify(t *testing.T) {
	secret := "test-secret-32-bytes-long!!!!!!"
	body := []byte(`{"event":"kora.work_order.after_save","data":{"name":"WO-0001"}}`)
	ts := time.Now().Unix()

	sig := Sign(secret, body, ts)
	if sig == "" {
		t.Fatal("Sign returned empty signature")
	}

	// Verify with correct secret.
	err := Verify(body, sig, []string{secret}, 5*time.Minute)
	if err != nil {
		t.Errorf("Verify failed with correct secret: %v", err)
	}
}

func TestVerifyWrongSecret(t *testing.T) {
	body := []byte(`{"test": true}`)
	ts := time.Now().Unix()
	sig := Sign("correct-secret-32-bytes-long", body, ts)

	err := Verify(body, sig, []string{"wrong-secret-32-bytes-long!"}, 5*time.Minute)
	if err == nil {
		t.Error("Verify should fail with wrong secret")
	}
}

func TestVerifyExpiredTimestamp(t *testing.T) {
	secret := "test-secret-32-bytes-long!!!!!!"
	body := []byte(`{"test": true}`)
	oldTs := time.Now().Add(-10 * time.Minute).Unix()

	sig := Sign(secret, body, oldTs)
	err := Verify(body, sig, []string{secret}, 5*time.Minute)
	if err == nil {
		t.Error("Verify should fail with expired timestamp")
	}
}

func TestVerifyMultiSecret(t *testing.T) {
	body := []byte(`{"test": true}`)
	ts := time.Now().Unix()

	// Sign with old secret.
	oldSig := Sign("old-secret-32-bytes-long!!!!!!", body, ts)

	// Verify with both old and new secrets (rotation scenario).
	err := Verify(body, oldSig, []string{"new-secret-32-bytes-long!!!!!", "old-secret-32-bytes-long!!!!!!"}, 5*time.Minute)
	if err != nil {
		t.Errorf("Verify with multi-secret failed: %v", err)
	}
}

func TestVerifyEmptyHeader(t *testing.T) {
	err := Verify([]byte(`{}`), "", []string{"secret"}, 5*time.Minute)
	if err == nil {
		t.Error("Verify should fail with empty header")
	}
}

func TestVerifyTamperedBody(t *testing.T) {
	secret := "test-secret-32-bytes-long!!!!!!"
	body := []byte(`{"amount": 100}`)
	ts := time.Now().Unix()

	sig := Sign(secret, body, ts)

	// Tamper with body.
	tampered := []byte(`{"amount": 999}`)
	err := Verify(tampered, sig, []string{secret}, 5*time.Minute)
	if err == nil {
		t.Error("Verify should fail with tampered body")
	}
}

func TestGenerateSecret(t *testing.T) {
	s1, err := GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret failed: %v", err)
	}
	if len(s1) != 64 { // hex-encoded 32 bytes
		t.Errorf("Secret length = %d, want 64", len(s1))
	}

	s2, _ := GenerateSecret()
	if s1 == s2 {
		t.Error("Generated secrets should be unique")
	}
}

func TestDefaultRetrySchedule(t *testing.T) {
	s := DefaultRetrySchedule()
	if len(s) != 8 {
		t.Errorf("Retry schedule length = %d, want 8", len(s))
	}
	if s[0] != 0 {
		t.Error("First attempt should be immediate")
	}
	if s[7] != 24*time.Hour {
		t.Errorf("Last retry = %v, want 24h", s[7])
	}
}
