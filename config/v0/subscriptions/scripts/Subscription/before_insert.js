// Copy a plan's current commercial terms to a new subscription. The operator
// can opt into a negotiated price by setting override_plan_terms.
var doc = __kora_event__.doc;
if (!doc.plan) {
  throw new Error("Choose a plan before creating a subscription.");
}

var plan = kora.getDoc("Plan", doc.plan);
if (!plan) {
  throw new Error("The selected plan could not be found.");
}
if (plan.status !== "Active") {
  throw new Error("Only an active plan can be used for a new subscription.");
}

if (!doc.override_plan_terms) {
  doc.unit_price = plan.unit_price;
  doc.currency = plan.currency;
  doc.billing_interval = plan.billing_interval;
  doc.interval_count = plan.interval_count || 1;
} else if (!(Number(doc.unit_price) > 0) || !(Number(doc.interval_count || 1) > 0)) {
  throw new Error("Enter a positive negotiated price and billing interval count.");
}

return { doc: doc };
