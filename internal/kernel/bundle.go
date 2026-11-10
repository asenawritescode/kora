package kernel

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/asenawritescode/kora/internal/doctype"
)

var createBundleReference = regexp.MustCompile(`\$records\.([A-Za-z0-9_-]+)\.name`)

type savedBundleDocument struct {
	dt                 *doctype.DocType
	doc                *doctype.Document
	oldDoc             *doctype.Document
	workflowTransition bool
}

func resolveCreateBundleData(raw json.RawMessage, priorNames map[string]string) (json.RawMessage, error) {
	var values map[string]any
	if len(raw) == 0 {
		values = map[string]any{}
	} else if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("data must be a JSON object")
	}
	resolved, err := resolveBundleValue(values, priorNames)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(resolved)
	if err != nil {
		return nil, fmt.Errorf("encode resolved data: %w", err)
	}
	return encoded, nil
}

func resolveBundleName(value string, names map[string]string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("bundle update name is required")
	}
	resolved, err := resolveBundleValue(value, names)
	if err != nil {
		return "", err
	}
	name, ok := resolved.(string)
	if !ok || strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("bundle update name is invalid")
	}
	return name, nil
}

func resolveBundleValue(value any, priorNames map[string]string) (any, error) {
	switch current := value.(type) {
	case string:
		var resolutionErr error
		resolved := createBundleReference.ReplaceAllStringFunc(current, func(token string) string {
			if resolutionErr != nil {
				return token
			}
			parts := createBundleReference.FindStringSubmatch(token)
			if len(parts) != 2 {
				resolutionErr = fmt.Errorf("invalid record reference %q", token)
				return token
			}
			name, ok := priorNames[parts[1]]
			if !ok {
				resolutionErr = fmt.Errorf("record reference %q must point to an earlier bundle item", token)
				return token
			}
			return name
		})
		return resolved, resolutionErr
	case map[string]any:
		resolved := make(map[string]any, len(current))
		for key, nested := range current {
			item, err := resolveBundleValue(nested, priorNames)
			if err != nil {
				return nil, err
			}
			resolved[key] = item
		}
		return resolved, nil
	case []any:
		resolved := make([]any, len(current))
		for index, nested := range current {
			item, err := resolveBundleValue(nested, priorNames)
			if err != nil {
				return nil, err
			}
			resolved[index] = item
		}
		return resolved, nil
	default:
		return value, nil
	}
}
