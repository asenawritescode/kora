package ai

import (
	"strings"
	"testing"

	"github.com/asenawritescode/kora/contract"
)

// weekFake answers the metrics routes with last week's sales, 112,480, against the week before, 103,700.
func weekFake() *fakeMetrics {
	fake := sampleFake()
	fake.answers[MetricsRouteMetrics] = okBody(`[{"name":"sales_total","label":"Sales","verified":true,"format":"currency","model":"Sale"},{"name":"credit_given","label":"Credit","verified":false}]`)
	fake.answers[MetricsRouteQuery] = MetricsResponse{Status: 200, Body: []byte(`{"data":{"metrics":["sales_total"],"dimensions":[],` +
		`"rows":[{"bucket":"2026-11-09","group":[],"values":["112480.00"]}],` +
		`"previous":[{"bucket":"2026-11-02","group":[],"values":["103700.00"]}]},` +
		`"meta":{"period":{"preset":"last_week","from":"2026-11-09","to":"2026-11-15"},` +
		`"previous":{"from":"2026-11-02","to":"2026-11-08"},"time_zone":"Africa/Nairobi","day_ends":"04:00","as_of":"2026-11-21T16:14:00Z"}}`)}
	return fake
}

func lastWeekTurn(t *testing.T) *answerTurn {
	t.Helper()
	turn := &answerTurn{run: weekFake().run, page: cardPage{Currency: "KES"}}
	out := executeMetricsTool(turn, toolQueryMetrics, map[string]any{"metrics": []any{"sales_total"}, "period": map[string]any{"preset": "last_week"}, "compare": "week_before", "grain": "range"})
	if strings.HasPrefix(out, "Error") {
		t.Fatalf("query refused: %s", out)
	}
	return turn
}

func TestAnswerCardPrintsFiguresFromTheResult(t *testing.T) {
	cards := lastWeekTurn(t).cards()
	if len(cards) != 1 {
		t.Fatalf("cards = %d, want 1", len(cards))
	}
	card := cards[0]
	if card.Kind != "number" || card.Label != "Sales" || !card.Verified {
		t.Fatalf("card = %+v", card)
	}
	if card.Value != "KES 112,480" || card.Previous != "KES 103,700" || card.Change != "▲ 8.5% on the week before" {
		t.Fatalf("figures = %q / %q / %q", card.Value, card.Previous, card.Change)
	}
	if card.Meta.Period != "9 Nov – 15 Nov 2026" || card.Meta.Compared != "2 Nov – 8 Nov 2026" || card.Meta.Zone != "Nairobi time" || card.Meta.DayEnds != "days end 04:00" {
		t.Fatalf("meta = %+v", card.Meta)
	}
	if !hasAction(card, "open_in_insights") || !hasAction(card, "see_records") || !hasAction(card, "save_as_report") {
		t.Fatalf("actions = %+v", card.Actions)
	}
	if hasAction(card, "pin_to_dashboard") {
		t.Fatal("a caller who is not the administrator was offered a pin")
	}
}

func TestPinIsOfferedOnlyToTheAdministrator(t *testing.T) {
	turn := lastWeekTurn(t)
	turn.page.CanShare = true
	card := turn.cards()[0]
	if !hasAction(card, "pin_to_dashboard") {
		t.Fatalf("administrator has no pin: %+v", card.Actions)
	}
	if !containsString(actionVisibility(card, "save_as_report"), "shop") {
		t.Fatal("administrator cannot save to the shop")
	}
}

func TestReplyMustBackEveryFigure(t *testing.T) {
	cases := []struct {
		name  string
		reply string
		ok    bool
	}{
		{"figures from the card", "Sales last week were KES 112,480, up 8.5% on the week before.", true},
		{"a figure the result lacks", "Sales last week were KES 112,481.", false},
		{"a percent the card does not print", "Sales are up 8.45%.", false},
		{"no figure at all", "Sales last week went up.", true},
		{"the period in the card's words", "Sales for 9 Nov – 15 Nov 2026 were KES 112,480.", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			turn := lastWeekTurn(t)
			got := len(turn.unbacked(tc.reply, "how did sales do last week?")) == 0
			if got != tc.ok {
				t.Fatalf("backed = %v, want %v", got, tc.ok)
			}
		})
	}
}

func TestWrongReplyIsReplacedWithANumberlessSentence(t *testing.T) {
	turn := lastWeekTurn(t)
	reply := turn.finalReply("Sales were KES 112,481 last week.", "how did sales do last week?")
	if reply != "Here are sales for last week." {
		t.Fatalf("final reply = %q", reply)
	}
	if len(turn.cards()) != 1 {
		t.Fatal("the card did not survive the replacement")
	}
}

func TestTurnWithoutMetricsHasNothingToCheck(t *testing.T) {
	turn := &answerTurn{run: sampleFake().run}
	if bad := turn.unbacked("You have 12 open orders.", "how many orders?"); len(bad) != 0 {
		t.Fatalf("turn without metrics checked numbers: %v", bad)
	}
}

func hasAction(card contract.AnswerCard, kind string) bool {
	return len(actionVisibilityFor(card, kind)) > 0 || actionKindPresent(card, kind)
}

func actionKindPresent(card contract.AnswerCard, kind string) bool {
	for _, a := range card.Actions {
		if a.Kind == kind {
			return true
		}
	}
	return false
}

func actionVisibility(card contract.AnswerCard, kind string) []string {
	return actionVisibilityFor(card, kind)
}

func actionVisibilityFor(card contract.AnswerCard, kind string) []string {
	for _, a := range card.Actions {
		if a.Kind == kind {
			return a.Visibility
		}
	}
	return nil
}

func containsString(in []string, want string) bool {
	for _, s := range in {
		if s == want {
			return true
		}
	}
	return false
}
