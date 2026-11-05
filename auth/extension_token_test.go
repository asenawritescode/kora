package auth

import "testing"

func TestHashExtensionAccessToken(t *testing.T) {
	got := HashExtensionAccessToken("abc")
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got != want {
		t.Fatalf("HashExtensionAccessToken(abc) = %s, want %s", got, want)
	}
	if HashExtensionAccessToken("abd") == got {
		t.Fatal("distinct tokens must not share a verifier")
	}
}
