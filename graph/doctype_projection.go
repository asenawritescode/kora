package graph

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
)

// ProjectDocTypes indexes the canonical DocType registry in a graph resource
// registry. The graph is a projection: it does not define, persist, or
// execute a second business-schema model. The supplied modelRevision is used
// as the resource version so a graph reference remains tied to the active
// configuration revision.
func ProjectDocTypes(ctx context.Context, target *Memory, source *doctype.Registry, namespace string, modelRevision int) ([]contract.ResourceRef, error) {
	if target == nil {
		return nil, fmt.Errorf("doctype projection target is required")
	}
	if source == nil {
		return nil, fmt.Errorf("doctype projection source is required")
	}
	if namespace == "" {
		return nil, contract.ErrResourceNamespaceRequired
	}
	if modelRevision <= 0 {
		return nil, fmt.Errorf("doctype projection model revision must be positive")
	}

	doctypes := source.All()
	sort.Slice(doctypes, func(i, j int) bool { return doctypes[i].Name < doctypes[j].Name })
	descriptors := make([]contract.ResourceDescriptor, 0, len(doctypes))
	for _, dt := range doctypes {
		if dt == nil || dt.Name == "" {
			return nil, fmt.Errorf("doctype projection contains an unnamed DocType")
		}
		fields := make([]contract.TypedField, 0, len(dt.Fields))
		for _, field := range dt.Fields {
			fields = append(fields, contract.TypedField{
				Name:     field.Fieldname,
				Type:     field.Fieldtype,
				Label:    field.Label,
				Required: field.Reqd,
				Unique:   field.Unique,
			})
		}
		descriptors = append(descriptors, contract.ResourceDescriptor{
			Ref:    contract.ResourceRef{Namespace: namespace, Name: dt.Name, Version: modelRevision},
			Kind:   contract.ResourceKindDoctype,
			Fields: fields,
		})
	}

	// Preflight every exact revision before mutating the target. This prevents
	// a later conflict from leaving a partially projected model active.
	for _, descriptor := range descriptors {
		existing, err := target.Resolve(descriptor.Ref)
		if err == nil {
			if existing.Hash != DescriptorHash(descriptor) {
				return nil, fmt.Errorf("project doctype %q: %w", descriptor.Ref.Name, contract.ErrResourceVersionConflict)
			}
			continue
		}
		if !errors.Is(err, contract.ErrResourceNotFound) {
			return nil, fmt.Errorf("inspect projected doctype %q: %w", descriptor.Ref.Name, err)
		}
	}

	refs := make([]contract.ResourceRef, 0, len(descriptors))
	for _, descriptor := range descriptors {
		ref, err := target.Register(ctx, descriptor)
		if err != nil {
			return nil, fmt.Errorf("project doctype %q: %w", descriptor.Ref.Name, err)
		}
		refs = append(refs, ref)
	}
	return refs, nil
}
