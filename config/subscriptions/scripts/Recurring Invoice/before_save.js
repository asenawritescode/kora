// An invoice takes its currency from its subscription and its subtotal from the billing
// schedule (or the subscription's amount per period) when left blank. A billing schedule is
// invoiced by the new invoice and goes back to Scheduled when the invoice is voided. The
// Engine derives computed fields before this script runs, so tax and total are worked out
// again here from the subtotal that was copied.
var doc = __kora_event__.doc;
var oldDoc = __kora_event__.oldDoc;

function money(value) {
  return Math.round(Number(value || 0) * 100) / 100;
}

function blank(value) {
  return value === null || value === undefined || String(value) === "";
}

if (oldDoc) {
  if (blank(doc.subtotal)) {
    throw new Error("Set a subtotal.");
  }
  if (oldDoc.status !== "Void" && doc.status === "Void" && !blank(doc.billing_schedule)) {
    var returned = kora.getDoc("Billing Schedule", doc.billing_schedule);
    if (!returned) {
      throw new Error("Cannot check billing schedule " + doc.billing_schedule + ".");
    }
    if (returned.status === "Invoiced") {
      kora.saveDoc("Billing Schedule", { name: returned.name, status: "Scheduled" });
    }
  }
  return { doc: doc };
}

var sub = kora.getDoc("Subscription", doc.subscription);
if (!sub) {
  throw new Error("Cannot check subscription " + doc.subscription + ".");
}
doc.currency = sub.currency;

var schedule = null;
if (!blank(doc.billing_schedule)) {
  schedule = kora.getDoc("Billing Schedule", doc.billing_schedule);
  if (!schedule) {
    throw new Error("Cannot check billing schedule " + doc.billing_schedule + ".");
  }
  var billRef = schedule.reference || schedule.name;
  if (String(schedule.subscription || "") !== String(doc.subscription || "")) {
    throw new Error("Billing " + billRef + " belongs to another subscription.");
  }
  if (schedule.status === "Invoiced") {
    throw new Error("Billing " + billRef + " is already Invoiced.");
  }
  if (schedule.status !== "Scheduled") {
    throw new Error("Billing " + billRef + " is " + schedule.status + "; only a scheduled billing can be invoiced.");
  }
}

if (money(doc.subtotal) === 0) {
  doc.subtotal = schedule ? money(schedule.amount) : money(sub.recurring_amount);
}
if (money(doc.subtotal) === 0) {
  throw new Error("Set a subtotal, or choose a billing schedule or subscription with an amount.");
}

var tax = money(money(doc.subtotal) * Number(doc.tax_rate || 0) / 100);
doc.tax_amount = tax;
doc.total = money(money(doc.subtotal) + tax);
doc.balance = money(doc.total - money(doc.amount_paid));

if (schedule) {
  kora.saveDoc("Billing Schedule", { name: schedule.name, status: "Invoiced" });
}

return { doc: doc };
