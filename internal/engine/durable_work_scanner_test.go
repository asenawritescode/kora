package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync/atomic"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/internal/analytics"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/dbaccess"
	knet "github.com/asenawritescode/kora/internal/net"
	"github.com/asenawritescode/kora/internal/site"
)

type durableScannerDirectory struct {
	page      []site.SiteDescriptor
	info      site.DBSiteInfo
	pageCalls *atomic.Int32
}

type scannerActivationDirectory struct {
	*tenantDirectoryStub
	page []site.SiteDescriptor
}

func (d *scannerActivationDirectory) GetDirectoryDescriptorPageContext(ctx context.Context, _ string, _ string, limit uint16) ([]site.SiteDescriptor, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(d.page) > int(limit) {
		return nil, errors.New("page exceeded requested limit")
	}
	return append([]site.SiteDescriptor(nil), d.page...), nil
}

func TestDurableWorkScannerRetriesColdActivationAfterResidentCapacityFrees(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	const scanKey = "engine:test:durable-work:default"
	const residentID = "resident-tenant"
	const coldID = "cold-work-tenant"
	resident := &knet.LoadedSite{SiteID: residentID, Name: "resident.example"}
	router := knet.NewSiteRouter([]*knet.LoadedSite{resident})
	handle, err := router.AcquireByID(context.Background(), residentID)
	if err != nil {
		t.Fatal("pin resident runtime:", err)
	}
	defer handle.Release()

	walDir := t.TempDir()
	tenantWAL := AnalyticsTenantWALDir(walDir, coldID)
	if err := os.MkdirAll(tenantWAL, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tenantWAL, "events.jsonl"), []byte("durable event"), 0o600); err != nil {
		t.Fatal(err)
	}
	baseDirectory := &tenantDirectoryStub{
		descriptor: site.SiteDescriptor{SiteID: coldID, Aliases: []string{"cold-work.example"}, Status: "active", ConfigRevision: 1},
		byID:       site.DBSiteInfo{SiteID: coldID, Name: "cold-work.example", Status: "active", ConfigRevision: 1},
		revision:   9,
	}
	directory := &scannerActivationDirectory{tenantDirectoryStub: baseDirectory, page: []site.SiteDescriptor{{SiteID: coldID, Status: "active"}}}
	var loads atomic.Int32
	coordinator, err := newTenantRuntimeCoordinator(TenantRuntimeCoordinatorConfig{
		Tenants: router, Directory: directory, Common: &site.CommonConfig{}, MaxResidentTenants: 1,
		Installer: func(site.DBSiteInfo, *knet.LoadedSite) error { return nil }, RetainRuntime: func(*knet.LoadedSite) {},
	}, func(_ context.Context, info site.DBSiteInfo, _ *site.CommonConfig, _ *knet.LoadedSite, _ string) (*knet.LoadedSite, error) {
		loads.Add(1)
		return &knet.LoadedSite{SiteID: info.SiteID, Name: info.Name, ConfigRevision: info.ConfigRevision, Config: knet.SiteRouterConfig{Hostname: info.Name}}, nil
	})
	if err != nil {
		t.Fatal("create coordinator:", err)
	}
	scanner, err := NewDurableWorkScanner(DurableWorkScannerConfig{
		ParentContext: context.Background(), PlatformDB: database, PlatformDialect: db.Resolve("libsql"),
		Directory: directory, Tenants: router, Coordinator: coordinator, Common: &site.CommonConfig{},
		Analytics: &analytics.Config{WALDir: walDir}, DirectoryConsumerID: "engine:test", PageSize: 2,
	})
	if err != nil {
		t.Fatal("create durable scanner:", err)
	}

	advanceSweep := func() {
		t.Helper()
		mock.ExpectQuery(regexp.QuoteMeta(`SELECT after_site_id,last_completed_at FROM _kora_durable_scan_cursor WHERE scan_key=?`)).
			WithArgs(scanKey).WillReturnRows(sqlmock.NewRows([]string{"after_site_id", "last_completed_at"}).AddRow("", nil))
		update := `UPDATE _kora_durable_scan_cursor SET after_site_id=?,last_completed_at=STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW'),updated_at=STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW') WHERE scan_key=? AND after_site_id=?`
		mock.ExpectExec(regexp.QuoteMeta(update)).WithArgs("", scanKey, "").WillReturnResult(sqlmock.NewResult(0, 1))
		if err := scanner.ScanOnce(context.Background()); err != nil {
			t.Fatalf("ScanOnce: %v", err)
		}
	}

	advanceSweep()
	if got := scanner.Snapshot(); got.CapacityRejections != 1 || got.DueTenants != 1 || loads.Load() != 0 {
		t.Fatalf("scanner after busy-resident rejection = %+v, loads=%d; want one capacity rejection and no activation", got, loads.Load())
	}
	advanceSweep()
	if got := scanner.Snapshot(); got.CapacityRejections != 2 || loads.Load() != 0 {
		t.Fatalf("scanner retry while still busy = %+v, loads=%d; want retry and no activation", got, loads.Load())
	}
	handle.Release()
	advanceSweep()
	if router.SnapshotByID(coldID) == nil || router.SnapshotByID(residentID) != nil || loads.Load() != 1 {
		t.Fatalf("cold activation after capacity frees: cold=%v resident=%v loads=%d", router.SnapshotByID(coldID), router.SnapshotByID(residentID), loads.Load())
	}
	if got := scanner.Snapshot(); got.CapacityRejections != 2 || got.DueTenants != 3 {
		t.Fatalf("scanner recovery metrics = %+v, want 2 capacity rejections and 3 due detections", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("scanner cursor expectations:", err)
	}
}

func TestDurableWorkScannerWakesColdTenantForAnalyticsWAL(t *testing.T) {
	base := t.TempDir()
	wal := AnalyticsTenantWALDir(base, "wal-tenant")
	if err := os.MkdirAll(wal, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wal, "events.jsonl.flushing"), []byte("durable event"), 0o600); err != nil {
		t.Fatal(err)
	}
	scanner := &DurableWorkScanner{
		directory: durableScannerDirectory{info: site.DBSiteInfo{SiteID: "wal-tenant", Name: "wal-tenant.example", Status: "active"}},
		common:    &site.CommonConfig{}, analytics: &analytics.Config{WALDir: base},
	}
	scanner.beginColdWALScan("")
	due, err := scanner.probeTenant(context.Background(), site.SiteDescriptor{SiteID: "wal-tenant", Status: "active"})
	if err != nil || !due {
		t.Fatalf("cold tenant with retained analytics WAL = (%t, %v), want (true, nil)", due, err)
	}
	scanner.publishColdWALSnapshot()
	backlog := scanner.Snapshot()
	if !backlog.ColdWALPending || !backlog.ColdWALAgeUnknown {
		t.Fatalf("cold tenant WAL metrics = %+v, want pending with unknown legacy/corrupt age", backlog)
	}
	if !backlog.ColdWALBytesKnown || backlog.ColdWALBytes != int64(len("durable event")) {
		t.Fatalf("cold tenant WAL byte metrics = %d known=%t, want %d known", backlog.ColdWALBytes, backlog.ColdWALBytesKnown, len("durable event"))
	}
}

func TestDurableWorkScannerRechecksAssignmentAfterReadingCurrentTenant(t *testing.T) {
	scanner := &DurableWorkScanner{
		directory: durableScannerDirectory{info: site.DBSiteInfo{
			SiteID: "tenant-moved", RuntimeCellID: "deployment-b", Status: "active",
		}},
		cellID: "deployment-a",
		common: &site.CommonConfig{},
	}
	due, err := scanner.probeTenant(context.Background(), site.SiteDescriptor{
		SiteID: "tenant-moved", RuntimeCellID: "deployment-a", Status: "active",
	})
	if err != nil || due {
		t.Fatalf("probe after assignment moved = (%t, %v), want (false, nil)", due, err)
	}
}

func TestDurableWorkScannerPublishesColdWALBytesOnlyForCompleteKnownSweep(t *testing.T) {
	t.Run("complete sweep sums bytes", func(t *testing.T) {
		scanner := &DurableWorkScanner{}
		scanner.beginColdWALScan("")
		scanner.recordColdWALBacklog(analytics.WALBacklogSnapshot{Bytes: 17, BytesKnown: true})
		scanner.recordColdWALBacklog(analytics.WALBacklogSnapshot{Bytes: 23, BytesKnown: true})
		scanner.publishColdWALSnapshot()
		got := scanner.Snapshot()
		if !got.ColdWALBytesKnown || got.ColdWALBytes != 40 {
			t.Fatalf("completed cold WAL bytes = %d known=%t, want 40 known", got.ColdWALBytes, got.ColdWALBytesKnown)
		}
	})
	t.Run("resumed sweep remains unknown", func(t *testing.T) {
		scanner := &DurableWorkScanner{}
		scanner.beginColdWALScan("tenant-previous-page")
		scanner.recordColdWALBacklog(analytics.WALBacklogSnapshot{Bytes: 23, BytesKnown: true})
		scanner.publishColdWALSnapshot()
		got := scanner.Snapshot()
		if got.ColdWALBytesKnown {
			t.Fatalf("resumed partial sweep bytes marked known: %+v", got)
		}
	})
	t.Run("failed probe marks sweep unknown", func(t *testing.T) {
		scanner := &DurableWorkScanner{}
		scanner.beginColdWALScan("")
		scanner.recordColdWALBacklog(analytics.WALBacklogSnapshot{Bytes: 17, BytesKnown: true})
		scanner.markColdWALBytesUnknown()
		scanner.publishColdWALSnapshot()
		got := scanner.Snapshot()
		if got.ColdWALBytesKnown {
			t.Fatalf("incomplete cold WAL sweep marked known: %+v", got)
		}
	})
}

func TestAnalyticsWALHasPendingIncludesCurrentAndRotatedRecords(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tenant")
	if pending, err := analytics.WALHasPending(dir); err != nil || pending {
		t.Fatalf("empty WAL state = (%t, %v), want (false, nil)", pending, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl.flushing"), []byte("pending"), 0o600); err != nil {
		t.Fatal(err)
	}
	if pending, err := analytics.WALHasPending(dir); err != nil || !pending {
		t.Fatalf("rotated WAL state = (%t, %v), want (true, nil)", pending, err)
	}
	if err := os.Remove(filepath.Join(dir, "events.jsonl.flushing")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte("pending"), 0o600); err != nil {
		t.Fatal(err)
	}
	if pending, err := analytics.WALHasPending(dir); err != nil || !pending {
		t.Fatalf("current WAL state = (%t, %v), want (true, nil)", pending, err)
	}
}

func (d durableScannerDirectory) GetDirectoryDescriptorPageContext(ctx context.Context, _ string, _ string, limit uint16) ([]site.SiteDescriptor, error) {
	if d.pageCalls != nil {
		d.pageCalls.Add(1)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(d.page) > int(limit) {
		return nil, errors.New("page exceeded requested limit")
	}
	return append([]site.SiteDescriptor(nil), d.page...), nil
}

type scannerWorkAdmission struct {
	calls    atomic.Int32
	active   atomic.Int32
	maxSeen  atomic.Int32
	reject   int32
	released atomic.Int32
}

func (a *scannerWorkAdmission) Acquire(ctx context.Context) (func(), error) {
	call := a.calls.Add(1)
	if call == a.reject {
		return nil, dbaccess.ErrExhausted
	}
	active := a.active.Add(1)
	for previous := a.maxSeen.Load(); active > previous && !a.maxSeen.CompareAndSwap(previous, active); previous = a.maxSeen.Load() {
	}
	var once atomic.Bool
	return func() {
		if once.CompareAndSwap(false, true) {
			a.active.Add(-1)
			a.released.Add(1)
		}
	}, nil
}

func TestDurableWorkScannerAdmitsEachPlatformOperation(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	const scanKey = "engine:test:durable-work:default"
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT after_site_id,last_completed_at FROM _kora_durable_scan_cursor WHERE scan_key=?`)).
		WithArgs(scanKey).WillReturnRows(sqlmock.NewRows([]string{"after_site_id", "last_completed_at"}).AddRow("", nil))
	update := `UPDATE _kora_durable_scan_cursor SET after_site_id=?,last_completed_at=STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW'),updated_at=STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW') WHERE scan_key=? AND after_site_id=?`
	mock.ExpectExec(regexp.QuoteMeta(update)).WithArgs("", scanKey, "").WillReturnResult(sqlmock.NewResult(0, 1))
	var pageCalls atomic.Int32
	directory := &durableScannerDirectory{page: []site.SiteDescriptor{{SiteID: "tenant-a", Status: "active"}}, pageCalls: &pageCalls}
	loaded := &knet.LoadedSite{SiteID: "tenant-a", Name: "tenant-a.example"}
	router := knet.NewSiteRouter([]*knet.LoadedSite{loaded})
	admission := &scannerWorkAdmission{}
	scanner, err := NewDurableWorkScanner(DurableWorkScannerConfig{
		ParentContext: context.Background(), PlatformDB: database, PlatformDialect: db.Resolve("libsql"),
		Directory: directory, Tenants: router, Coordinator: testTenantCoordinator(t, router, nil),
		Common: &site.CommonConfig{}, DirectoryConsumerID: "engine:test", PageSize: 2, WorkAdmission: admission,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := scanner.ScanOnce(context.Background()); err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}
	if got := admission.calls.Load(); got != 3 {
		t.Fatalf("platform admission calls = %d, want one each for cursor load, directory page and cursor advance", got)
	}
	if got := admission.released.Load(); got != 3 || admission.active.Load() != 0 || admission.maxSeen.Load() != 1 {
		t.Fatalf("admission released=%d active=%d max-concurrent=%d, want 3/0/1", got, admission.active.Load(), admission.maxSeen.Load())
	}
	if got := pageCalls.Load(); got != 1 {
		t.Fatalf("directory page calls = %d, want 1", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDurableWorkScannerAdmissionRejectionStopsPlatformOperation(t *testing.T) {
	for rejectAt := int32(1); rejectAt <= 3; rejectAt++ {
		t.Run(fmt.Sprintf("operation_%d", rejectAt), func(t *testing.T) {
			database, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			const scanKey = "engine:test:durable-work:default"
			if rejectAt > 1 {
				mock.ExpectQuery(regexp.QuoteMeta(`SELECT after_site_id,last_completed_at FROM _kora_durable_scan_cursor WHERE scan_key=?`)).
					WithArgs(scanKey).WillReturnRows(sqlmock.NewRows([]string{"after_site_id", "last_completed_at"}).AddRow("", nil))
			}
			var pageCalls atomic.Int32
			directory := &durableScannerDirectory{page: []site.SiteDescriptor{{SiteID: "tenant-a", Status: "active"}}, pageCalls: &pageCalls}
			loaded := &knet.LoadedSite{SiteID: "tenant-a", Name: "tenant-a.example"}
			router := knet.NewSiteRouter([]*knet.LoadedSite{loaded})
			admission := &scannerWorkAdmission{reject: rejectAt}
			scanner, err := NewDurableWorkScanner(DurableWorkScannerConfig{
				ParentContext: context.Background(), PlatformDB: database, PlatformDialect: db.Resolve("libsql"),
				Directory: directory, Tenants: router, Coordinator: testTenantCoordinator(t, router, nil),
				Common: &site.CommonConfig{}, DirectoryConsumerID: "engine:test", PageSize: 2, WorkAdmission: admission,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := scanner.ScanOnce(context.Background()); !errors.Is(err, dbaccess.ErrExhausted) {
				t.Fatalf("ScanOnce error = %v, want database work admission rejection", err)
			}
			if got := admission.calls.Load(); got != rejectAt {
				t.Fatalf("admission calls = %d, want rejection on operation %d", got, rejectAt)
			}
			if got, want := pageCalls.Load(), rejectAt > 2; (got == 1) != want {
				t.Fatalf("directory page calls = %d, want page run=%t", got, want)
			}
			if admission.active.Load() != 0 || admission.released.Load() != rejectAt-1 {
				t.Fatalf("admission active=%d released=%d, want 0/%d", admission.active.Load(), admission.released.Load(), rejectAt-1)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDurableWorkScannerAdmissionRejectsStartupSchemaCheck(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	router := knet.NewSiteRouter(nil)
	admission := &scannerWorkAdmission{reject: 1}
	scanner, err := NewDurableWorkScanner(DurableWorkScannerConfig{
		ParentContext: context.Background(), PlatformDB: database, PlatformDialect: db.Resolve("libsql"),
		Directory: durableScannerDirectory{}, Tenants: router, Coordinator: testTenantCoordinator(t, router, nil),
		Common: &site.CommonConfig{}, DirectoryConsumerID: "engine:test", WorkAdmission: admission,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := scanner.Start(); !errors.Is(err, dbaccess.ErrExhausted) {
		t.Fatalf("Start error = %v, want schema admission rejection", err)
	}
	if admission.calls.Load() != 1 || admission.active.Load() != 0 {
		t.Fatalf("startup admission calls=%d active=%d, want 1/0", admission.calls.Load(), admission.active.Load())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDurableWorkScannerAdmissionRejectsTenantDescriptorLookup(t *testing.T) {
	admission := &scannerWorkAdmission{reject: 1}
	scanner := &DurableWorkScanner{
		directory: durableScannerDirectory{info: site.DBSiteInfo{SiteID: "tenant-a", Name: "tenant-a.example", Status: "active"}},
		common:    &site.CommonConfig{}, workAdmission: admission,
	}
	if _, err := scanner.probeTenant(context.Background(), site.SiteDescriptor{SiteID: "tenant-a", Status: "active"}); !errors.Is(err, dbaccess.ErrExhausted) {
		t.Fatalf("probeTenant error = %v, want descriptor lookup admission rejection", err)
	}
	if admission.calls.Load() != 1 || admission.active.Load() != 0 {
		t.Fatalf("descriptor lookup admission calls=%d active=%d, want 1/0", admission.calls.Load(), admission.active.Load())
	}
}

func TestColdTenantProbeAdmissionRejectsBeforeSQL(t *testing.T) {
	admission := &scannerWorkAdmission{reject: 1}
	called := false
	due, err := withTenantProbeAdmission(context.Background(), admission, func() (bool, error) {
		called = true
		return false, nil
	})
	if due || !errors.Is(err, dbaccess.ErrExhausted) || called {
		t.Fatalf("guard result = (%t, %v, called=%t), want exhausted without SQL", due, err, called)
	}
	if admission.active.Load() != 0 || admission.released.Load() != 0 {
		t.Fatalf("rejected admission active=%d released=%d, want 0/0", admission.active.Load(), admission.released.Load())
	}
}

func TestColdTenantProbeAdmissionReleasesAfterSQL(t *testing.T) {
	admission := &scannerWorkAdmission{}
	called := false
	due, err := withTenantProbeAdmission(context.Background(), admission, func() (bool, error) {
		called = true
		if admission.active.Load() != 1 {
			t.Errorf("active admission inside probe = %d, want 1", admission.active.Load())
		}
		return true, nil
	})
	if err != nil || !due || !called || admission.calls.Load() != 1 || admission.released.Load() != 1 || admission.active.Load() != 0 {
		t.Fatalf("guard result=(%t, %v, called=%t) admission calls=%d released=%d active=%d", due, err, called, admission.calls.Load(), admission.released.Load(), admission.active.Load())
	}
}

func (d durableScannerDirectory) GetByIDContext(context.Context, string) (site.DBSiteInfo, error) {
	if d.info.SiteID != "" {
		return d.info, nil
	}
	return site.DBSiteInfo{}, errors.New("unexpected cold-tenant probe")
}

func TestDurableWorkScannerAdvancesBoundedKeysetPage(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	const scanKey = "engine:test:durable-work:default"
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT after_site_id,last_completed_at FROM _kora_durable_scan_cursor WHERE scan_key=?`)).
		WithArgs(scanKey).WillReturnRows(sqlmock.NewRows([]string{"after_site_id", "last_completed_at"}).AddRow("", nil))
	update := `UPDATE _kora_durable_scan_cursor SET after_site_id=?,last_completed_at=STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW'),updated_at=STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW') WHERE scan_key=? AND after_site_id=?`
	mock.ExpectExec(regexp.QuoteMeta(update)).WithArgs("", scanKey, "").WillReturnResult(sqlmock.NewResult(0, 1))

	loaded := &knet.LoadedSite{SiteID: "tenant-a", Name: "tenant-a.example"}
	router := knet.NewSiteRouter([]*knet.LoadedSite{loaded})
	scanner, err := NewDurableWorkScanner(DurableWorkScannerConfig{
		ParentContext: context.Background(), PlatformDB: database, PlatformDialect: db.Resolve("libsql"),
		Directory: durableScannerDirectory{page: []site.SiteDescriptor{{SiteID: "tenant-a", Status: "active"}}},
		Tenants:   router, Coordinator: testTenantCoordinator(t, router, nil), Common: &site.CommonConfig{},
		DirectoryConsumerID: "engine:test", PageSize: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := scanner.ScanOnce(context.Background()); err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}
	if got := scanner.Snapshot(); got.Probes != 0 || got.DueTenants != 0 || got.LastCompletedSweep.IsZero() {
		t.Fatalf("snapshot after a completed page = %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDurableScanStateRejectsCursorMovedByAnotherScanner(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	state, err := newDurableScanStateStore(database, db.Resolve("libsql"))
	if err != nil {
		t.Fatal(err)
	}
	update := `UPDATE _kora_durable_scan_cursor SET after_site_id=?,updated_at=STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW') WHERE scan_key=? AND after_site_id=?`
	mock.ExpectExec(regexp.QuoteMeta(update)).WithArgs("tenant-z", "scan", "tenant-a").WillReturnResult(sqlmock.NewResult(0, 0))
	if err := state.Advance(context.Background(), "scan", "tenant-a", "tenant-z", false); !errors.Is(err, ErrDurableScanCursorMoved) {
		t.Fatalf("Advance error = %v, want cursor moved", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestActiveScanDescriptorHonorsCellAndStatus(t *testing.T) {
	cases := []struct {
		name       string
		descriptor site.SiteDescriptor
		cell       string
		want       bool
	}{
		{name: "active default", descriptor: site.SiteDescriptor{SiteID: "a", Status: "active"}, want: true},
		{name: "degraded assigned", descriptor: site.SiteDescriptor{SiteID: "b", Status: "degraded", RuntimeCellID: "west"}, cell: "west", want: true},
		{name: "other cell", descriptor: site.SiteDescriptor{SiteID: "c", Status: "active", RuntimeCellID: "east"}, cell: "west"},
		{name: "paused", descriptor: site.SiteDescriptor{SiteID: "d", Status: "paused"}},
		{name: "missing identity", descriptor: site.SiteDescriptor{Status: "active"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := activeScanDescriptor(tc.descriptor, tc.cell); got != tc.want {
				t.Fatalf("activeScanDescriptor = %t, want %t", got, tc.want)
			}
		})
	}
}
