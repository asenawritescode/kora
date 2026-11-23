package metrics

// Part is one metric a derived or ratio metric is made of. Sign is "+" or "-" for a term of a sum or
// difference; Role is "numerator" or "denominator" for a ratio. A part sets exactly one of the two.
type Part struct {
	Metric string
	Sign   string
	Role   string
}

// Parts lists the metrics this metric is made of, in formula order. A ratio's parts are its numerator and
// denominator. A derived formula has parts when it is a flat sum or difference of metric references (each
// signed), or one division of two metric references (numerator and denominator). Any other formula, such as
// one with a number or a product, has no parts. Validation parses every formula, so a parse failure is
// reported as no parts here.
func (m Metric) Parts() []Part {
	switch m.Type {
	case KindRatio:
		return []Part{{Metric: m.Numerator, Role: "numerator"}, {Metric: m.Denominator, Role: "denominator"}}
	case KindDerived:
		root, err := parseFormula(m.Formula)
		if err != nil {
			return nil
		}
		return root.parts()
	case KindSimple, KindBalance, KindCumulative:
		return nil
	}
	return nil
}

// parts reads the parts of a parsed derived formula; see Metric.Parts.
func (n *formulaNode) parts() []Part {
	if n.op == '/' && n.left.isRef() && n.right.isRef() {
		return []Part{{Metric: n.left.ref, Role: "numerator"}, {Metric: n.right.ref, Role: "denominator"}}
	}
	out, ok := n.signedParts("+")
	if !ok {
		return nil
	}
	return out
}

// isRef reports whether the node is a bare metric reference.
func (n *formulaNode) isRef() bool { return n.op == 0 && n.ref != "" }

// signedParts lists the metric references of a flat sum or difference, each with its sign: sign is the sign
// of this node. The right operand of a minus takes the opposite sign. It reports false for any node that is
// not a reference or a + or - of such nodes: a number, a product or a quotient.
func (n *formulaNode) signedParts(sign string) ([]Part, bool) {
	if n.isRef() {
		return []Part{{Metric: n.ref, Sign: sign}}, true
	}
	if n.op != '+' && n.op != '-' {
		return nil, false
	}
	left, ok := n.left.signedParts(sign)
	if !ok {
		return nil, false
	}
	rightSign := sign
	if n.op == '-' {
		rightSign = "-"
		if sign == "-" {
			rightSign = "+"
		}
	}
	right, ok := n.right.signedParts(rightSign)
	if !ok {
		return nil, false
	}
	return append(left, right...), true
}
