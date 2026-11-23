package site

import (
	"strings"
	"testing"
)

func TestParsePackRetiredViews(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		want    []string
		wantErr string
	}{
		{name: "absent", body: "name: shop\nversion: \"1\"\n", want: []string{}},
		{name: "listed", body: "name: shop\nversion: \"1\"\nretires:\n  views: [owner_dashboard, old_list]\n", want: []string{"owner_dashboard", "old_list"}},
		{name: "blank name rejected", body: "name: shop\nversion: \"1\"\nretires:\n  views: [\"  \"]\n", wantErr: "blank view"},
		{name: "unknown key rejected", body: "name: shop\nversion: \"1\"\nretires:\n  screens: [x]\n", wantErr: "parse pack.yaml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePackRetiredViews([]byte(tc.body))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestValidatePackRejectsRetiredViewThePackStillShips(t *testing.T) {
	view := PackFile{Path: "views/owner_dashboard.yaml", ContentType: "view", Content: "name: owner_dashboard\nroute: /dashboard\ntype: dashboard\n"}
	displayView := PackFile{Path: "views/owner_dashboard.yaml", ContentType: "view", Content: "name: Owner Dashboard\nroute: /dashboard\ntype: dashboard\n"}
	manifest := func(retires string) PackFile {
		return PackFile{Path: "pack.yaml", ContentType: "pack", Content: "name: shop\nversion: \"1\"\n" + retires}
	}
	for _, tc := range []struct {
		name     string
		files    []PackFile
		wantCode string
	}{
		{name: "retired and shipped", files: []PackFile{view, manifest("retires:\n  views: [owner_dashboard]\n")}, wantCode: "retired_view_shipped"},
		{name: "stored display name retired by snake case and shipped", files: []PackFile{displayView, manifest("retires:\n  views: [owner_dashboard]\n")}, wantCode: "retired_view_shipped"},
		{name: "stored display name retired by its own name and shipped", files: []PackFile{displayView, manifest("retires:\n  views: [\"Owner Dashboard\"]\n")}, wantCode: "retired_view_shipped"},
		{name: "retired and not shipped", files: []PackFile{manifest("retires:\n  views: [owner_dashboard]\n")}},
		{name: "invalid retires", files: []PackFile{manifest("retires:\n  views: [\" \"]\n")}, wantCode: "retires_invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := ValidatePack(PackManifest{SchemaVersion: 1, Name: "shop", Version: "1"}, tc.files)
			if err != nil {
				t.Fatal(err)
			}
			var codes []string
			for _, f := range result.Findings {
				codes = append(codes, f.Code)
			}
			if tc.wantCode == "" {
				if !result.Valid {
					t.Fatalf("expected valid pack, findings %v", codes)
				}
				return
			}
			if result.Valid || !strings.Contains(strings.Join(codes, ","), tc.wantCode) {
				t.Fatalf("findings %v, want code %s", codes, tc.wantCode)
			}
		})
	}
}

func TestParseConfigPackCarriesRetiredViews(t *testing.T) {
	snapshot, _, err := ParseConfigPack("shop", []PackFile{{Path: "pack.yaml", ContentType: "pack", Content: "name: shop\nversion: \"1\"\nretires:\n  views: [owner_dashboard]\n"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(snapshot.RetiredViews, ",") != "owner_dashboard" {
		t.Fatalf("RetiredViews = %v, want [owner_dashboard]", snapshot.RetiredViews)
	}
}
