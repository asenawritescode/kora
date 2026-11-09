package outbox

import "strings"

// EventCategory classifies a durable event for docs, dashboards, and
// adapter-specific routing. The category is descriptive only; delivery behavior
// remains governed by the outbox and publisher contracts.
type EventCategory string

const (
	EventCategoryTechnical   EventCategory = "technical"
	EventCategoryDomain      EventCategory = "domain"
	EventCategoryOperational EventCategory = "operational"
	EventCategoryUnknown     EventCategory = "unknown"
)

// ClassifyEventType maps a canonical event type to a coarse category.
func ClassifyEventType(eventType string) EventCategory {
	switch {
	case strings.HasPrefix(eventType, "kora.") && (strings.Contains(eventType, ".after_insert") ||
		strings.Contains(eventType, ".after_save") || strings.Contains(eventType, ".after_delete") ||
		strings.Contains(eventType, ".after_submit") || strings.Contains(eventType, ".after_cancel")):
		return EventCategoryTechnical
	case strings.HasSuffix(eventType, ".failed") || strings.HasPrefix(eventType, "deployment.") || strings.HasPrefix(eventType, "component."):
		return EventCategoryOperational
	case strings.HasPrefix(eventType, "kora.") || strings.Contains(eventType, ".submitted") || strings.Contains(eventType, ".received"):
		return EventCategoryDomain
	default:
		return EventCategoryUnknown
	}
}
