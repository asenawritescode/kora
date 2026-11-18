package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/internal/tenanttime"
)

func TestSiteSettingsUpdatesValidatePresentFieldsOnly(t *testing.T) {
	zone, cutoff, currency := "Europe/London", " 02:00 ", "usd"
	badZone, badCutoff, badCurrency := "Mars/Olympus", "07:00", "XXX"
	for _, tt := range []struct {
		name    string
		request siteSettingsRequest
		want    []siteSetting
		field   string // expected failing field, "" when valid
	}{
		{name: "empty request changes nothing", request: siteSettingsRequest{}},
		{name: "currency alone", request: siteSettingsRequest{Currency: &currency},
			want: []siteSetting{{"currency", "USD"}}},
		{name: "zone and cutoff are normalised", request: siteSettingsRequest{TimeZone: &zone, BusinessDayCutoff: &cutoff},
			want: []siteSetting{{tenanttime.KeyTimeZone, "Europe/London"}, {tenanttime.KeyBusinessDayCutoff, "02:00"}}},
		{name: "unknown zone", request: siteSettingsRequest{TimeZone: &badZone}, field: "time_zone"},
		{name: "cutoff past 06:00", request: siteSettingsRequest{BusinessDayCutoff: &badCutoff}, field: "business_day_cutoff"},
		{name: "unsupported currency", request: siteSettingsRequest{Currency: &badCurrency}, field: "currency"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.request.updates()
			if tt.field != "" {
				var settingErr *settingError
				if !errors.As(err, &settingErr) || settingErr.field != tt.field {
					t.Fatalf("updates() error = %v, want field %s", err, tt.field)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("updates() = %#v, want %#v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("updates()[%d] = %#v, want %#v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestSiteSettingsPutSavesOnlyPresentFields(t *testing.T) {
	for _, tt := range []struct{ name, body, key, value string }{
		{"currency alone", `{"currency":"ugx"}`, "currency", "UGX"},
		{"time zone alone", `{"time_zone":"Europe/London"}`, tenanttime.KeyTimeZone, "Europe/London"},
		{"cutoff alone", `{"business_day_cutoff":"03:00"}`, tenanttime.KeyBusinessDayCutoff, "03:00"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, mock := adminGuardRouter(t, []string{"Administrator"}, RegisterSystemRoutes)
			mock.ExpectBegin()
			mock.ExpectExec("INSERT INTO _kora_site_setting").WithArgs("s1", tt.key, tt.value).
				WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectCommit()
			for _, key := range []string{"currency", tenanttime.KeyTimeZone, tenanttime.KeyBusinessDayCutoff} {
				rows := sqlmock.NewRows([]string{"setting_value"})
				if key == tt.key {
					rows.AddRow(tt.value)
				}
				mock.ExpectQuery("SELECT setting_value FROM _kora_site_setting").WithArgs("s1", key).WillReturnRows(rows)
			}
			w := serve(r, http.MethodPut, "/api/system/settings", tt.body)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
			}
			var body struct {
				Data map[string]any `json:"data"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Data[tt.key] != tt.value {
				t.Fatalf("data[%s] = %v, want %s", tt.key, body.Data[tt.key], tt.value)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSiteSettingsPutRejectsInvalidFieldsWithoutWriting(t *testing.T) {
	for _, tt := range []struct{ name, body, field string }{
		{"unknown zone", `{"time_zone":"Mars/Olympus"}`, "time_zone"},
		{"local zone is not a tenant zone", `{"time_zone":"Local"}`, "time_zone"},
		{"cutoff past 06:00", `{"business_day_cutoff":"07:00"}`, "business_day_cutoff"},
		{"malformed cutoff", `{"business_day_cutoff":"3am"}`, "business_day_cutoff"},
		{"unsupported currency", `{"currency":"XXX"}`, "currency"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := adminGuardRouter(t, []string{"Administrator"}, RegisterSystemRoutes)
			w := serve(r, http.MethodPut, "/api/system/settings", tt.body)
			var body struct {
				Error struct {
					Details map[string]any `json:"details"`
				} `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if w.Code != http.StatusBadRequest || body.Error.Details["field"] != tt.field {
				t.Fatalf("status = %d body = %s, want 400 field %s", w.Code, w.Body.String(), tt.field)
			}
		})
	}
}
