// A subscription follows its plan. A new subscription takes the plan's currency, billing
// interval, interval count and (when its price is blank or 0) unit price. A new or re-planned
// subscription needs an Active plan. The amount per period is worked out here because the
// Engine derives computed fields before this script runs.
var doc = __kora_event__.doc;
var oldDoc = __kora_event__.oldDoc;

function money(value) {
  return Math.round(Number(value || 0) * 100) / 100;
}

function blank(value) {
  return value === null || value === undefined || String(value) === "";
}

var planChanged = !!oldDoc && String(doc.plan || "") !== String(oldDoc.plan || "");
var plan = null;
if (!oldDoc || planChanged) {
  plan = kora.getDoc("Plan", doc.plan);
  if (!plan) {
    throw new Error("Cannot check plan " + doc.plan + ".");
  }
  if (plan.status !== "Active") {
    throw new Error("Plan " + (plan.plan_name || plan.name) + " is " + plan.status + "; choose an active plan.");
  }
}

if (!oldDoc) {
  doc.currency = plan.currency;
  doc.billing_interval = plan.billing_interval;
  doc.interval_count = Number(plan.interval_count);
  if (money(doc.unit_price) === 0) {
    doc.unit_price = money(plan.unit_price);
  }
  if (money(doc.unit_price) === 0) {
    throw new Error("Set a unit price or choose a plan with one.");
  }
  doc.recurring_amount = money(Number(doc.quantity || 0) * money(doc.unit_price) + money(doc.lines_total));
} else if (blank(doc.unit_price)) {
  throw new Error("Set a unit price.");
}

return { doc: doc };
