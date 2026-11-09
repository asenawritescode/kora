package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/site"
	"github.com/gin-gonic/gin"
)

type managementErrorEnvelope struct {
	Error *contract.Error `json:"error"`
	Meta  *Meta           `json:"meta,omitempty"`
}

func writeManagementError(c *gin.Context, status int, code contract.Code, message string) {
	c.JSON(status, managementErrorEnvelope{Error: contract.NewError(code, message)})
}

// ManagementDirectory is the credential-free subset of Engine's platform
// directory needed by the management inspection API.
type ManagementDirectory interface {
	GetDescriptorByIDContext(context.Context, string) (site.Descriptor, error)
	ResolveDescriptorByAliasContext(context.Context, string) (site.Descriptor, error)
	CurrentDirectoryRevisionContext(context.Context) (uint64, error)
}

type TenantConfigRevisionResolver func(context.Context, string) (uint64, error)

// ManagementInspectHandler serves a safe platform projection. Mutating
// management endpoints are intentionally registered only when their durable
// operation executors are part of Engine composition.
type ManagementInspectHandler struct {
	directory     ManagementDirectory
	tokenHash     [32]byte
	deployment    contract.DeploymentID
	workAdmission interface {
		Acquire(context.Context) (func(), error)
	}
	configRevision TenantConfigRevisionResolver
}

var ErrManagementCapacity = errors.New("management platform work capacity exhausted")
var ErrManagementDirectoryChanged = errors.New("tenant directory changed during routing lookup")

func NewManagementInspectHandler(directory ManagementDirectory, token, deployment string, admissions ...interface {
	Acquire(context.Context) (func(), error)
}) (*ManagementInspectHandler, error) {
	if directory == nil {
		return nil, errors.New("management directory is required")
	}
	if strings.TrimSpace(token) == "" || strings.ContainsAny(token, "\r\n") {
		return nil, errors.New("management bearer token is required")
	}
	parsed, err := contract.ParseDeploymentID(strings.TrimSpace(deployment))
	if err != nil {
		return nil, errors.New("valid Engine deployment ID is required")
	}
	handler := &ManagementInspectHandler{directory: directory, tokenHash: sha256.Sum256([]byte(token)), deployment: parsed}
	if len(admissions) > 0 {
		handler.workAdmission = admissions[0]
	}
	return handler, nil
}

func (h *ManagementInspectHandler) Register(router gin.IRouter) {
	group := router.Group("/api/management/v2")
	group.Use(h.authenticate)
	group.GET("/tenants/:id", h.inspect)
	group.GET("/tenants/resolve", h.resolveAlias)
	group.GET("/directory/resolve", h.resolveDeploymentEntry)
	group.POST("/packages/validate", h.validatePack)
}

// resolveDeploymentEntry returns one authoritative routing projection. The
// revision is read on both sides of descriptor resolution so a concurrent
// placement update fails closed instead of returning a mixed directory view.
func (h *ManagementInspectHandler) resolveDeploymentEntry(c *gin.Context) {
	siteID := strings.TrimSpace(c.Query("site_id"))
	alias := strings.TrimSpace(c.Query("alias"))
	if (siteID == "") == (alias == "") {
		writeManagementError(c, http.StatusBadRequest, contract.CodeValidationFailed, "exactly one of site_id or alias is required")
		return
	}
	var tenant contract.TenantID
	if siteID != "" {
		parsed, err := contract.ParseTenantID(siteID)
		if err != nil {
			writeManagementError(c, http.StatusBadRequest, contract.CodeValidationFailed, "invalid tenant ID")
			return
		}
		tenant = parsed
	} else {
		alias = site.NormalizeSiteAlias(alias)
		if err := contract.ValidateTenantAlias(alias); err != nil {
			writeManagementError(c, http.StatusBadRequest, contract.CodeValidationFailed, "valid tenant alias is required")
			return
		}
	}

	var entry contract.SiteDirectoryEntryV2
	err := h.withWork(c.Request.Context(), func() error {
		before, err := h.directory.CurrentDirectoryRevisionContext(c.Request.Context())
		if err != nil {
			return err
		}
		var descriptor site.Descriptor
		if tenant != "" {
			descriptor, err = h.directory.GetDescriptorByIDContext(c.Request.Context(), string(tenant))
		} else {
			descriptor, err = h.directory.ResolveDescriptorByAliasContext(c.Request.Context(), alias)
		}
		if err != nil {
			return err
		}
		after, err := h.directory.CurrentDirectoryRevisionContext(c.Request.Context())
		if err != nil {
			return err
		}
		if before != after {
			return ErrManagementDirectoryChanged
		}
		entry, err = site.DeploymentEntryV2FromDescriptor(descriptor, after)
		return err
	})
	if err != nil {
		if errors.Is(err, ErrManagementDirectoryChanged) {
			c.Header("Retry-After", "1")
			writeManagementError(c, http.StatusServiceUnavailable, contract.CodeResourceExhausted, "tenant placement changed; retry the routing lookup")
			return
		}
		h.writeDirectoryError(c, err)
		return
	}
	c.JSON(http.StatusOK, Response{Data: entry})
}

// SetTenantConfigRevisionResolver installs the Engine-owned tenant DB reader.
// The platform directory revision remains independent for lifecycle CAS.
func (h *ManagementInspectHandler) SetTenantConfigRevisionResolver(resolve TenantConfigRevisionResolver) {
	if h != nil {
		h.configRevision = resolve
	}
}

type managementPackValidationRequest struct {
	Manifest contract.PackManifest `json:"manifest"`
	Files    []contract.PackFile   `json:"files"`
}

func (h *ManagementInspectHandler) validatePack(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 3<<20)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	var input managementPackValidationRequest
	if err := decoder.Decode(&input); err != nil {
		writeManagementError(c, http.StatusBadRequest, contract.CodeValidationFailed, "package validation request is invalid or exceeds 3 MiB")
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		writeManagementError(c, http.StatusBadRequest, contract.CodeValidationFailed, "package validation request must contain exactly one JSON value")
		return
	}
	if len(input.Files) > 256 {
		writeManagementError(c, http.StatusBadRequest, contract.CodeValidationFailed, "package validation request contains too many files")
		return
	}
	var validation contract.PackValidation
	err := h.withWork(c.Request.Context(), func() error {
		var validateErr error
		validation, validateErr = site.ValidatePack(input.Manifest, input.Files)
		return validateErr
	})
	if err != nil {
		if errors.Is(err, ErrManagementCapacity) {
			h.writeDirectoryError(c, err)
		} else {
			writeManagementError(c, http.StatusServiceUnavailable, contract.CodeDependencyUnavailable, "Engine package validation is unavailable")
		}
		return
	}
	if len(validation.Findings) > 100 {
		writeManagementError(c, http.StatusUnprocessableEntity, contract.CodeValidationFailed, "package validation produced too many findings; correct the first issues before retrying")
		return
	}
	for i := range validation.Findings {
		validation.Findings[i].Code = truncateManagementText(validation.Findings[i].Code, 80)
		validation.Findings[i].Path = truncateManagementText(validation.Findings[i].Path, 256)
		validation.Findings[i].Message = truncateManagementText(validation.Findings[i].Message, 512)
	}
	c.JSON(http.StatusOK, Response{Data: validation})
}

func truncateManagementText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}

func (h *ManagementInspectHandler) authenticate(c *gin.Context) {
	parts := strings.SplitN(c.GetHeader("Authorization"), " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		writeManagementError(c, http.StatusUnauthorized, contract.CodeUnauthenticated, "management bearer credential required")
		c.Abort()
		return
	}
	provided := sha256.Sum256([]byte(parts[1]))
	if subtle.ConstantTimeCompare(provided[:], h.tokenHash[:]) != 1 {
		writeManagementError(c, http.StatusUnauthorized, contract.CodeUnauthenticated, "invalid management bearer credential")
		c.Abort()
		return
	}
	c.Next()
}

func (h *ManagementInspectHandler) inspect(c *gin.Context) {
	tenant, err := contract.ParseTenantID(c.Param("id"))
	if err != nil {
		writeManagementError(c, http.StatusBadRequest, contract.CodeValidationFailed, "invalid tenant ID")
		return
	}
	var descriptor site.Descriptor
	err = h.withWork(c.Request.Context(), func() error {
		var readErr error
		descriptor, readErr = h.directory.GetDescriptorByIDContext(c.Request.Context(), string(tenant))
		return readErr
	})
	if err != nil {
		h.writeDirectoryError(c, err)
		return
	}
	h.writeDescriptor(c, tenant, descriptor)
}

func (h *ManagementInspectHandler) resolveAlias(c *gin.Context) {
	alias := site.NormalizeSiteAlias(c.Query("alias"))
	if err := contract.ValidateTenantAlias(alias); err != nil {
		writeManagementError(c, http.StatusBadRequest, contract.CodeValidationFailed, "valid tenant alias is required")
		return
	}
	var descriptor site.Descriptor
	err := h.withWork(c.Request.Context(), func() error {
		var readErr error
		descriptor, readErr = h.directory.ResolveDescriptorByAliasContext(c.Request.Context(), alias)
		return readErr
	})
	if err != nil {
		h.writeDirectoryError(c, err)
		return
	}
	tenant, err := contract.ParseTenantID(descriptor.SiteID)
	if err != nil {
		writeManagementError(c, http.StatusServiceUnavailable, contract.CodeDependencyUnavailable, "tenant directory contains an invalid identity")
		return
	}
	h.writeDescriptor(c, tenant, descriptor)
}

func (h *ManagementInspectHandler) writeDescriptor(c *gin.Context, tenant contract.TenantID, descriptor site.Descriptor) {
	var directoryRevision uint64
	err := h.withWork(c.Request.Context(), func() error {
		var readErr error
		directoryRevision, readErr = h.directory.CurrentDirectoryRevisionContext(c.Request.Context())
		return readErr
	})
	if err != nil {
		h.writeDirectoryError(c, err)
		return
	}
	status := contract.TenantState(strings.ToLower(strings.TrimSpace(descriptor.Status)))
	if status == "" {
		status = contract.TenantActive
	}
	result := contract.TenantDescriptorV2{
		Version: 2, SiteID: tenant, Aliases: descriptor.Aliases, Status: status,
		ConfigRevision:    contract.Revision(strconv.FormatUint(descriptor.ConfigRevision, 10)),
		DirectoryRevision: contract.Revision(strconv.FormatUint(directoryRevision, 10)), Deployment: h.deployment,
		DataPlaneRef: descriptor.DataPlaneRef,
	}
	if h.configRevision != nil && status == contract.TenantActive {
		var tenantConfigRevision uint64
		err := h.withWork(c.Request.Context(), func() error {
			var readErr error
			tenantConfigRevision, readErr = h.configRevision(c.Request.Context(), string(tenant))
			return readErr
		})
		if err != nil || tenantConfigRevision == 0 {
			writeManagementError(c, http.StatusServiceUnavailable, contract.CodeDependencyUnavailable, "tenant configuration revision is unavailable")
			return
		}
		result.TenantConfigRevision = contract.Revision(strconv.FormatUint(tenantConfigRevision, 10))
	}
	if err := contract.ValidateTenantDescriptorV2(result); err != nil {
		writeManagementError(c, http.StatusServiceUnavailable, contract.CodeDependencyUnavailable, "tenant directory contains metadata that cannot be represented by the management contract")
		return
	}
	c.JSON(http.StatusOK, Response{Data: result})
}

func (h *ManagementInspectHandler) withWork(ctx context.Context, work func() error) error {
	if h.workAdmission == nil {
		return work()
	}
	release, err := h.workAdmission.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrManagementCapacity, err)
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return err
	}
	return work()
}

func (h *ManagementInspectHandler) writeDirectoryError(c *gin.Context, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		writeManagementError(c, http.StatusNotFound, contract.CodeNotFound, "tenant not found")
		return
	}
	if errors.Is(err, ErrManagementCapacity) {
		c.Header("Retry-After", "1")
		writeManagementError(c, http.StatusServiceUnavailable, contract.CodeResourceExhausted, "platform work capacity is temporarily exhausted")
		return
	}
	writeManagementError(c, http.StatusServiceUnavailable, contract.CodeDependencyUnavailable, "tenant directory is unavailable")
}
