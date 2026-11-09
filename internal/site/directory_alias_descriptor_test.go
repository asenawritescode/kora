package site

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestResolveDescriptorByAliasUsesOnlyCredentialFreeDirectoryColumns(t *testing.T) {
	for _, test := range []struct {
		name          string
		dialect       string
		aliasQuery    string
		descriptorSQL string
	}{
		{
			name: "mysql", dialect: "mysql",
			aliasQuery:    `SELECT site_id FROM _kora_site_alias WHERE alias = ? ORDER BY site_id LIMIT 2`,
			descriptorSQL: `SELECT site_id, COALESCE(runtime_cell_id, ''), site, db_type, db_host, db_port, db_name, COALESCE(domains_json, '[]'), COALESCE(file_storage, 'local'), COALESCE(storage_bucket, ''), status, config_revision FROM _kora_site_registry WHERE site_id = ?`,
		},
		{
			name: "postgres", dialect: "postgres",
			aliasQuery:    `SELECT site_id FROM _kora_site_alias WHERE alias = $1 ORDER BY site_id LIMIT 2`,
			descriptorSQL: `SELECT site_id, COALESCE(runtime_cell_id, ''), site, db_type, db_host, db_port, db_name, COALESCE(domains_json, '[]'), COALESCE(file_storage, 'local'), COALESCE(storage_bucket, ''), status, config_revision FROM _kora_site_registry WHERE site_id = $1`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			mock.ExpectQuery(regexp.QuoteMeta(`SELECT initialized FROM _kora_site_alias_meta WHERE id = 1`)).
				WillReturnRows(sqlmock.NewRows([]string{"initialized"}).AddRow(1))
			mock.ExpectQuery(regexp.QuoteMeta(test.aliasQuery)).WithArgs("acme.example.test").
				WillReturnRows(sqlmock.NewRows([]string{"site_id"}).AddRow("tenant-a"))
			mock.ExpectQuery(regexp.QuoteMeta(test.descriptorSQL)).WithArgs("tenant-a").WillReturnRows(
				sqlmock.NewRows([]string{"site_id", "runtime_cell_id", "site", "db_type", "db_host", "db_port", "db_name", "domains_json", "file_storage", "storage_bucket", "status", "config_revision"}).
					AddRow("tenant-a", "", "acme.example.test", "mysql", "db.internal", 3306, "tenant_db", `["acme.example.test"]`, "local", "", "active", 3),
			)

			descriptor, err := NewSQLSiteRegistry(db, test.dialect).ResolveDescriptorByAliasContext(context.Background(), " ACME.EXAMPLE.TEST. ")
			if err != nil {
				t.Fatalf("ResolveDescriptorByAliasContext() error = %v", err)
			}
			if descriptor.SiteID != "tenant-a" || descriptor.Status != "active" || descriptor.DataPlaneRef == "" || strings.Contains(descriptor.DataPlaneRef, "db.internal") || len(descriptor.Aliases) != 1 || descriptor.Aliases[0] != "acme.example.test" {
				t.Fatalf("unexpected descriptor: %+v", descriptor)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestResolveDescriptorByAliasHonorsCanceledContext(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = NewSQLSiteRegistry(db, "mysql").ResolveDescriptorByAliasContext(ctx, "acme.example.test")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ResolveDescriptorByAliasContext() error = %v, want context.Canceled", err)
	}
}

func TestDeploymentEntryV2UsesLegacyAssignmentAsSingleSource(t *testing.T) {
	for _, test := range []struct {
		name string
		cell string
		want string
	}{
		{name: "default assignment", cell: "", want: "default"},
		{name: "existing cell assignment", cell: "cell-west", want: "cell-west"},
	} {
		t.Run(test.name, func(t *testing.T) {
			entry, err := DeploymentEntryV2FromDescriptor(Descriptor{
				SiteID: "tenant-a", Aliases: []string{"shop.example.test"}, Status: "active",
				ConfigRevision: 9, RuntimeCellID: test.cell, DataPlaneRef: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			}, 9007199254740993)
			if err != nil {
				t.Fatalf("DeploymentEntryV2FromDescriptor() error = %v", err)
			}
			if string(entry.DeploymentID) != test.want || entry.ConfigRevision != "9" || entry.DirectoryRevision != "9007199254740993" {
				t.Fatalf("v2 entry = %+v", entry)
			}
		})
	}
}

func TestDeploymentEntryV2FailsClosedOnInvalidLegacyAssignment(t *testing.T) {
	_, err := DeploymentEntryV2FromDescriptor(Descriptor{
		SiteID: "tenant-a", Aliases: []string{"shop.example.test"}, Status: "active", RuntimeCellID: "cell/unsafe",
	}, 1)
	if err == nil {
		t.Fatal("invalid legacy assignment was emitted as a deployment ID")
	}
}

func TestValidateDeploymentDirectoryV2RejectsInvalidAndDuplicateAssignments(t *testing.T) {
	valid := Descriptor{SiteID: "tenant-a", Aliases: []string{"shop.example.test"}, Status: "active", RuntimeCellID: "cell-west"}
	if err := ValidateDeploymentDirectoryV2([]Descriptor{valid}, 7); err != nil {
		t.Fatalf("valid deployment directory rejected: %v", err)
	}

	invalid := valid
	invalid.RuntimeCellID = "cell/west"
	if err := ValidateDeploymentDirectoryV2([]Descriptor{invalid}, 7); err == nil {
		t.Fatal("invalid assignment was accepted by directory validation")
	}
	if err := ValidateDeploymentDirectoryV2([]Descriptor{valid, valid}, 7); err == nil {
		t.Fatal("duplicate tenant assignment was accepted by directory validation")
	}
}
