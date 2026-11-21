package kernel

import (
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/shopspring/decimal"
)

func d(v int64) decimal.Decimal { return decimal.NewFromInt(v) }

func TestMovingAverage(t *testing.T) {
	for _, tc := range []struct {
		name             string
		oldQty, oldAvg   decimal.Decimal
		addQty, addValue decimal.Decimal
		want             decimal.Decimal
	}{
		{"nothing on hand takes the cost", d(0), d(0), d(10), d(500), d(50)},
		{"10 at 50 and 10 at 70 average 60", d(10), d(50), d(10), d(700), d(60)},
		{"negative quantity counts as zero", d(-5), d(40), d(10), d(700), d(70)},
		{"20 at 120 onto 10 at 60 average 100", d(10), d(60), d(20), d(2400), d(100)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := movingAverage(tc.oldQty, tc.oldAvg, costMove{qty: tc.addQty, value: tc.addValue})
			if !got.Equal(tc.want) {
				t.Fatalf("average = %s, want %s", got, tc.want)
			}
		})
	}
}

func receiptEffect(average *doctype.AverageCost, direction string) *doctype.QuantityEffect {
	return &doctype.QuantityEffect{
		TargetDocType: "Product", TargetQuantityField: "stock_qty", LinesField: "items",
		SourceRecordField: "product", DeltaField: "quantity", Direction: direction,
		AverageCost: average,
	}
}

func receiptLine(product string, quantity float64, cost any) *doctype.Document {
	line := doctype.NewDocument("Receipt Line")
	line.Set("product", product)
	line.Set("quantity", quantity)
	if cost != nil {
		line.Set("unit_cost", cost)
	}
	return line
}

func receiptWith(lines ...*doctype.Document) *doctype.Document {
	receipt := doctype.NewDocument("Receipt")
	receipt.SetTable("items", lines)
	return receipt
}

func TestCostMoveSumsTheRowsOfEachTarget(t *testing.T) {
	average := &doctype.AverageCost{TargetField: "cost_price", CostField: "unit_cost"}
	effect := receiptEffect(average, "increase")
	for name, tc := range map[string]struct {
		doc  *doctype.Document
		want map[string]costMove
	}{
		"two lines of one product at different costs": {
			receiptWith(receiptLine("P", 10, 50.0), receiptLine("P", 10, 70.0)),
			map[string]costMove{"P": {qty: d(20), value: d(1200)}},
		},
		"a blank cost line does not count": {
			receiptWith(receiptLine("P", 10, 50.0), receiptLine("P", 30, nil)),
			map[string]costMove{"P": {qty: d(10), value: d(500)}},
		},
		"a zero quantity line adds nothing": {
			receiptWith(receiptLine("P", 0, 50.0)),
			map[string]costMove{},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := costMoveOf(effect, tc.doc)
			if len(got) != len(tc.want) {
				t.Fatalf("moves = %v, want %v", got, tc.want)
			}
			for target, want := range tc.want {
				if !got[target].qty.Equal(want.qty) || !got[target].value.Equal(want.value) {
					t.Fatalf("%s move = %+v, want %+v", target, got[target], want)
				}
			}
		})
	}
}

func TestCostMoveIsNilForADecreaseOrWithoutAverage(t *testing.T) {
	doc := receiptWith(receiptLine("P", 10, 50.0))
	if got := costMoveOf(receiptEffect(&doctype.AverageCost{TargetField: "cost_price", CostField: "unit_cost"}, "decrease"), doc); len(got) != 0 {
		t.Fatalf("decrease moved the average: %v", got)
	}
	if got := costMoveOf(receiptEffect(nil, "increase"), doc); len(got) != 0 {
		t.Fatalf("effect without average_cost moved the average: %v", got)
	}
}
