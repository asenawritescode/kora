package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/gin-gonic/gin"
)

func TestValidateDenominations(t *testing.T) {
	for _, tt := range []struct {
		name  string
		lines []denomination
		want  string // expected code, "" when valid
	}{
		{"kes list is valid", defaultDenominations["KES"], ""},
		{"empty list is valid", nil, ""},
		{"zero value", []denomination{{"0", kindCoin}}, "validation.invalid_denomination"},
		{"negative value", []denomination{{"-5", kindCoin}}, "validation.invalid_denomination"},
		{"not a number", []denomination{{"five", kindCoin}}, "validation.invalid_denomination"},
		{"unknown kind", []denomination{{"10", "bill"}}, "validation.invalid_denomination_kind"},
		{"duplicate value", []denomination{{"10", kindCoin}, {"10.0", kindCoin}}, "validation.denomination_order"},
		{"ascending order", []denomination{{"5", kindCoin}, {"10", kindCoin}}, "validation.denomination_order"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := validateDenominations(tt.lines)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("validateDenominations() = %v, want valid", err)
				}
				return
			}
			var settingErr *settingError
			if !errors.As(err, &settingErr) || settingErr.code != tt.want || settingErr.field != "denominations" {
				t.Fatalf("validateDenominations() = %v, want code %s", err, tt.want)
			}
		})
	}
}

func TestValidateDenominationsCanonicalises(t *testing.T) {
	got, err := validateDenominations([]denomination{{"2.50", " COIN "}, {"0.5", "Note"}})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Value != "2.5" || got[0].Kind != kindCoin || got[1].Value != "0.5" || got[1].Kind != kindNote {
		t.Fatalf("canonical rows = %#v", got)
	}
}

func TestDenominationDefaults(t *testing.T) {
	kes := defaultDenominations["KES"]
	if len(kes) != 9 {
		t.Fatalf("KES defaults = %d rows, want 9", len(kes))
	}
	for i, want := range []string{"1000", "500", "200", "100", "50"} {
		if string(kes[i].Value) != want || kes[i].Kind != kindNote {
			t.Fatalf("KES[%d] = %#v, want note %s", i, kes[i], want)
		}
	}
	for i, want := range []string{"20", "10", "5", "1"} {
		if string(kes[5+i].Value) != want || kes[5+i].Kind != kindCoin {
			t.Fatalf("KES[%d] = %#v, want coin %s", 5+i, kes[5+i], want)
		}
	}
	if _, ok := defaultDenominations["UGX"]; ok {
		t.Fatal("UGX should start with no built-in denominations")
	}
}

func TestDenominationsForReadsStoredThenDefault(t *testing.T) {
	for _, tt := range []struct {
		name, currency string
		stored         *string
		wantLen        int
	}{
		{"unset KES uses built-in default", "KES", nil, 9},
		{"unset currency without default is empty", "UGX", nil, 0},
		{"stored list wins", "UGX", strPtr(`[{"value":"2000","kind":"note"}]`), 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			rows := sqlmock.NewRows([]string{"setting_value"})
			if tt.stored != nil {
				rows.AddRow(*tt.stored)
			}
			mock.ExpectQuery("SELECT setting_value FROM _kora_site_setting").WithArgs("s1", "denominations."+tt.currency).WillReturnRows(rows)
			gin.SetMode(gin.TestMode)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodGet, "/api/system/settings", nil)
			c.Set("site_db", db)
			c.Set("site_name", "s1")
			c.Set("site_registry", doctype.NewRegistry())
			got, err := NewHandler(nil, nil).denominationsFor(c, tt.currency)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != tt.wantLen {
				t.Fatalf("denominationsFor(%s) = %#v, want %d rows", tt.currency, got, tt.wantLen)
			}
		})
	}
}

func TestSiteSettingsPutStoresDenominations(t *testing.T) {
	r, mock := adminGuardRouter(t, []string{"Administrator"}, RegisterSystemRoutes)
	mock.ExpectQuery("SELECT setting_value FROM _kora_site_setting").WithArgs("s1", "currency").
		WillReturnRows(sqlmock.NewRows([]string{"setting_value"}).AddRow("UGX"))
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO _kora_site_setting").WithArgs("s1", "denominations.UGX", `[{"value":1000,"kind":"note"},{"value":50,"kind":"coin"}]`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectQuery("SELECT setting_value FROM _kora_site_setting").WithArgs("s1", "currency").
		WillReturnRows(sqlmock.NewRows([]string{"setting_value"}).AddRow("UGX"))
	for _, key := range []string{"time_zone", "business_day_cutoff"} {
		mock.ExpectQuery("SELECT setting_value FROM _kora_site_setting").WithArgs("s1", key).
			WillReturnRows(sqlmock.NewRows([]string{"setting_value"}))
	}
	mock.ExpectQuery("SELECT setting_value FROM _kora_site_setting").WithArgs("s1", "denominations.UGX").
		WillReturnRows(sqlmock.NewRows([]string{"setting_value"}).AddRow(`[{"value":1000,"kind":"note"},{"value":50,"kind":"coin"}]`))
	w := serve(r, http.MethodPut, "/api/system/settings", `{"denominations":[{"value":1000,"kind":"note"},{"value":"50","kind":"COIN"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	var body struct {
		Data struct {
			Denominations []denomination `json:"denominations"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Data.Denominations) != 2 || body.Data.Denominations[1].Kind != kindCoin {
		t.Fatalf("data.denominations = %#v", body.Data.Denominations)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSiteSettingsPutRejectsBadDenominationsWithoutWriting(t *testing.T) {
	r, mock := adminGuardRouter(t, []string{"Administrator"}, RegisterSystemRoutes)
	mock.ExpectQuery("SELECT setting_value FROM _kora_site_setting").WithArgs("s1", "currency").
		WillReturnRows(sqlmock.NewRows([]string{"setting_value"}))
	w := serve(r, http.MethodPut, "/api/system/settings", `{"denominations":[{"value":"10","kind":"coin"},{"value":"20","kind":"coin"}]}`)
	var body struct {
		Error struct {
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusBadRequest || body.Error.Details["field"] != "denominations" {
		t.Fatalf("status = %d body = %s, want 400 field denominations", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
