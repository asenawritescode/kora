package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/dbaccess"
	"github.com/asenawritescode/kora/internal/site"
)

// PlatformSiteDirectory is the validated platform directory snapshot Engine
// acquires before assembling tenant runtimes. Engine owns DB until ownership
// transfers to ProcessResources.
type PlatformSiteDirectory struct {
	DB          *sql.DB
	Directory   *site.SQLSiteRegistry
	StartCursor uint64
	CellID      string
	Sites       []site.DBSiteInfo
	Descriptors []contract.SiteDescriptor
}

// OpenPlatformSiteDirectory opens and validates the platform database and
// discovers credential-free tenant routing metadata. Any failure closes the acquired DB
// and joins a close failure with the primary startup error.
func OpenPlatformSiteDirectory(driver, dsn, dialectName, rawCellID string) (PlatformSiteDirectory, error) {
	return openPlatformSiteDirectoryWithOpener(driver, dsn, dialectName, rawCellID, dbaccess.Open)
}

func openPlatformSiteDirectoryWithOpener(driver, dsn, dialectName, rawCellID string, open func(string, string) (*sql.DB, error)) (_ PlatformSiteDirectory, err error) {
	database, err := open(driver, dsn)
	if err != nil {
		slog.Error("startup db check: failed to open", "type", driver, "error", err)
		return PlatformSiteDirectory{}, fmt.Errorf("failed to open %s connection: %w", driver, err)
	}
	defer func() {
		if err != nil && database != nil {
			if closeErr := database.Close(); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("close platform database after startup failure: %w", closeErr))
			}
		}
	}()
	if err = database.Ping(); err != nil {
		return PlatformSiteDirectory{}, fmt.Errorf("failed to ping %s: %w", driver, err)
	}
	maxOpen, err := platformPoolLimitFromEnv()
	if err != nil {
		return PlatformSiteDirectory{}, err
	}
	tunePlatformDBPool(database, driver, maxOpen)
	slog.Info("database connected", "type", driver)
	if err = site.BootstrapPlatformRegistry(context.Background(), database, kdb.Resolve(dialectName)); err != nil {
		return PlatformSiteDirectory{}, fmt.Errorf("bootstrapping platform registry: %w", err)
	}
	directory := site.NewSQLSiteRegistry(database, dialectName)
	cursor, err := directory.CurrentDirectoryRevision()
	if err != nil {
		return PlatformSiteDirectory{}, fmt.Errorf("reading site directory startup revision: %w", err)
	}
	cellID, err := site.NormalizeRuntimeCellID(rawCellID)
	if err != nil {
		return PlatformSiteDirectory{}, fmt.Errorf("invalid KORA_ENGINE_CELL_ID: %w", err)
	}
	descriptors, err := directory.GetDirectoryDescriptors()
	if err != nil {
		return PlatformSiteDirectory{}, fmt.Errorf("credential-free site discovery from platform directory failed: %w", err)
	}
	if err := site.ValidateDeploymentDirectoryV2(descriptors, cursor); err != nil {
		return PlatformSiteDirectory{}, fmt.Errorf("validating platform deployment assignments: %w", err)
	}
	return PlatformSiteDirectory{DB: database, Directory: directory, StartCursor: cursor, CellID: cellID, Descriptors: descriptors}, nil
}

func tunePlatformDBPool(db *sql.DB, driver string, maxOpen int) {
	db.SetMaxOpenConns(maxOpen)
	switch driver {
	case "libsql":
		db.SetMaxIdleConns(0)
		db.SetConnMaxLifetime(25 * time.Second)
		db.SetConnMaxIdleTime(20 * time.Second)
	case "mysql":
		db.SetMaxIdleConns(5)
		db.SetConnMaxIdleTime(2 * time.Minute)
		db.SetConnMaxLifetime(10 * time.Minute)
	}
}

const defaultPlatformDBMaxOpen = 4

func platformPoolLimitFromEnv() (int, error) {
	value := strings.TrimSpace(os.Getenv("KORA_PLATFORM_DB_MAX_OPEN"))
	if value == "" {
		return defaultPlatformDBMaxOpen, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 || parsed > 256 {
		return 0, fmt.Errorf("KORA_PLATFORM_DB_MAX_OPEN must be an integer from 1 through 256")
	}
	return parsed, nil
}
