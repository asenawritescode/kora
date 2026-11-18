package outbox

import "testing"

func TestStatusTransitions(t *testing.T) {
	tests := []struct {
		name     string
		from, to Status
		allowed  bool
	}{
		{"claim pending", StatusPending, StatusPublishing, true},
		{"lease takeover", StatusPublishing, StatusPublishing, true},
		{"publish", StatusPublishing, StatusPublished, true},
		{"retry", StatusPublishing, StatusPending, true},
		{"fail on attempts", StatusPublishing, StatusFailed, true},
		{"block on capacity", StatusPublishing, StatusBlocked, true},
		{"blocked retry", StatusBlocked, StatusPending, true},
		{"operator replay", StatusFailed, StatusPending, true},
		{"pending cannot publish directly", StatusPending, StatusPublished, false},
		{"pending cannot block", StatusPending, StatusBlocked, false},
		{"published is terminal", StatusPublished, StatusPending, false},
		{"blocked cannot fail", StatusBlocked, StatusFailed, false},
		{"failed cannot publish", StatusFailed, StatusPublished, false},
		{"unknown source", Status("bogus"), StatusPending, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := transition(tc.from, tc.to)
			if (err == nil) != tc.allowed {
				t.Fatalf("transition(%s, %s) error = %v, want allowed=%v", tc.from, tc.to, err, tc.allowed)
			}
		})
	}
}
