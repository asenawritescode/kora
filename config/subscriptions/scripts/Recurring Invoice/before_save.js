// An invoice takes its currency from its subscription and, when its subtotal is blank or 0,
// the billing schedule's amount (or the subscription's amount per period). A new invoice that
// is not Void invoices its billing schedule. A Draft invoice can move to another schedule: the
// old one goes back to Scheduled and the new one is invoiced. Voiding an invoice returns its
// schedule to Scheduled. The Engine derives computed fields before this script runs, so tax,
// total and balance are worked out again here from the subtotal that was copied.
var doc = __kora_event__.doc;
var oldDoc = __kora_event__.oldDoc;

function money(value) {
  return Math.round(Number(value || 0) * 100) / 100;
}

function blank(value) {
  return value === null || value === undefined || String(value) === "";
}

function readSchedule(name) {
  var found = kora.getDoc("Billing Schedule", name);
  if (!found) {
    throw new Error("Cannot check billing schedule " + name + ".");
  }
  return found;
}

// invoiceSchedule checks that a schedule can be invoiced for this subscription, then makes it Invoiced.
function invoiceSchedule(schedule, subscription) {
  var billRef = schedule.reference || schedule.name;
  if (String(schedule.subscription || "") !== String(subscription || "")) {
    throw new Error("Billing " + billRef + " belongs to another subscription.");
  }
  if (schedule.status === "Invoiced") {
    throw new Error("Billing " + billRef + " is already Invoiced.");
  }
  if (schedule.status !== "Scheduled") {
    throw new Error("Billing " + billRef + " is " + schedule.status + "; only a scheduled billing can be invoiced.");
  }
  kora.saveDoc("Billing Schedule", { name: schedule.name, status: "Invoiced" });
}

// returnSchedule puts an invoiced schedule back to Scheduled.
function returnSchedule(name) {
  var schedule = readSchedule(name);
  if (schedule.status === "Invoiced") {
    kora.saveDoc("Billing Schedule", { name: schedule.name, status: "Scheduled" });
  }
}

if (oldDoc) {
  if (blank(doc.subtotal)) {
    throw new Error("Set a subtotal.");
  }
  var scheduleChanged = String(doc.billing_schedule || "") !== String(oldDoc.billing_schedule || "");
  if (scheduleChanged) {
    if (oldDoc.status !== "Draft" || doc.status !== "Draft") {
      throw new Error("The billing schedule of an invoice cannot be changed; void it and make a new one.");
    }
    if (!blank(oldDoc.billing_schedule)) {
      returnSchedule(oldDoc.billing_schedule);
    }
    if (!blank(doc.billing_schedule)) {
      invoiceSchedule(readSchedule(doc.billing_schedule), doc.subscription);
    }
  }
  if (oldDoc.status !== "Void" && doc.status === "Void" && !blank(doc.billing_schedule)) {
    returnSchedule(doc.billing_schedule);
  }
  return { doc: doc };
}

var sub = kora.getDoc("Subscription", doc.subscription);
if (!sub) {
  throw new Error("Cannot check subscription " + doc.subscription + ".");
}
doc.currency = sub.currency;

var schedule = blank(doc.billing_schedule) ? null : readSchedule(doc.billing_schedule);
var isVoid = doc.status === "Void";
if (schedule && !isVoid) {
  invoiceSchedule(schedule, doc.subscription);
}

if (money(doc.subtotal) === 0) {
  var source = schedule ? schedule.amount : sub.recurring_amount;
  if (blank(source)) {
    throw new Error("Set a subtotal, or choose a billing schedule or subscription with an amount.");
  }
  doc.subtotal = money(source);
}

var tax = money(money(doc.subtotal) * Number(doc.tax_rate || 0) / 100);
doc.tax_amount = tax;
doc.total = money(money(doc.subtotal) + tax);
doc.balance = money(doc.total - money(doc.amount_paid));

return { doc: doc };
