# Kigae SDK for Go

Go types and interfaces for building script extensions, provider integrations, and tools for Kigae.

## Installation

```bash
go get github.com/asenawritescode/kora/sdk@latest
```

## Usage

```go
import "github.com/asenawritescode/kora/sdk"
```

The SDK provides interfaces that scripts and extensions implement against:

### Tenant management client

Cloud and self-hosted operators use the typed HTTP client in
`github.com/asenawritescode/kora/sdk/management`. It accepts a service token
source, uses bounded HTTP response reads, validates commands and response
identity, and requires HTTPS except for loopback development endpoints. The
token source must provide a management-scoped credential; managed-channel
tenant credentials are not interchangeable.

```go
import (
    "context"

    "github.com/asenawritescode/kora/contract"
    "github.com/asenawritescode/kora/sdk/management"
)

client, err := management.NewClient(management.Config{
    BaseURL: "https://engine.example.test",
    Tokens: management.TokenSourceFunc(func(context.Context) (string, error) {
        return managementToken, nil
    }),
})
if err != nil {
    return err
}
operation, err := client.ApplyPackage(ctx, contract.ApplyPackage{
    Meta: contract.CommandMeta{IdempotencyKey: operationKey},
    Tenant: tenantID,
    ArtifactRef: artifactRef,
    Digest: packageDigest,
    ExpectedConfigRevision: configRevision,
})
```

The client targets `/api/management/v2`; it does not make these capabilities
available until the Engine exposes their authenticated, durable handlers.

### DocProvider — CRUD on Kigae documents

```go
type DocProvider interface {
    GetDoc(doctype, name string) (map[string]any, error)
    GetList(doctype string, filters map[string]any, orderBy string, limit, offset int) ([]map[string]any, error)
    SaveDoc(doctype string, doc map[string]any, modifiedBy string) error
    CreateDoc(doctype string, doc map[string]any, owner, modifiedBy string) (map[string]any, error)
    DeleteDoc(doctype, name string) error
}
```

### SecretProvider — retrieve API keys and secrets

```go
type SecretProvider interface {
    GetSecret(key string) (string, error)
}
```

### HTTPProvider — make external HTTP requests from scripts

```go
type HTTPProvider interface {
    DoHTTP(req *HTTPRequest) (*HTTPResponse, error)
}
```

### KoraProvider — composed interface for script contexts

```go
type KoraProvider interface {
    DocProvider
    SecretProvider
    HTTPProvider
}
```

### Script support

Pre-defined constants for supported script types and events (`BeforeInsert`, `AfterInsert`, `BeforeSave`, `AfterSave`, `BeforeDelete`, `AfterDelete`).

### Extension types

`ExtensionRecord` and `DeliveryRecord` structs for webhook extension management and delivery tracking.

## License

MIT
