package cli

import "testing"

func TestSplitHTTPOriginsTrimsAndDeduplicates(t *testing.T) {
	got := splitHTTPOrigins(" https://cloud.example.test, ,https://studio.example.test,https://cloud.example.test ")
	if len(got) != 2 || got[0] != "https://cloud.example.test" || got[1] != "https://studio.example.test" {
		t.Fatalf("splitHTTPOrigins() = %#v", got)
	}
}
