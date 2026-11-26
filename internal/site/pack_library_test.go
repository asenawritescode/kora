package site

import "testing"

func TestPackLibraryName(t *testing.T) {
	tests := []struct {
		path     string
		wantName string
		wantOK   bool
	}{
		{path: "scripts/lib/util.js", wantName: "util", wantOK: true},
		{path: "scripts/lib/posting.js", wantName: "posting", wantOK: true},
		{path: "scripts/lib/util.txt", wantOK: false},
		{path: "scripts/lib/nested/util.js", wantOK: false},
		{path: "scripts/Supplier Bill/before_save.js", wantOK: false},
		{path: "scripts/util.js", wantOK: false},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			name, ok := packLibraryName(tc.path)
			if ok != tc.wantOK || name != tc.wantName {
				t.Fatalf("packLibraryName(%q) = (%q, %t), want (%q, %t)", tc.path, name, ok, tc.wantName, tc.wantOK)
			}
		})
	}
}

func TestPackScriptBindingIgnoresLibraries(t *testing.T) {
	if doctypeName, eventName := packScriptBinding("scripts/lib/before_save.js"); doctypeName != "" || eventName != "" {
		t.Fatalf("library file bound to %q/%q, want unbound", doctypeName, eventName)
	}
}
