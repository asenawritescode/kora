// Credit sales and register sales. A sale entered as Awaiting Payment is a credit
// sale: its Payment records settle it. A register sale is written as Paid (or Held,
// then Paid) with its Sale Payment lines, and owes nothing. The sale owns the
// customer's outstanding_balance: each save moves it by what the sale owed before
// and what it owes now, so a payment reaches the customer through its sale.
var doc = __kora_event__.doc;
var oldDoc = __kora_event__.oldDoc;

function money(value) {
  return Math.round(Number(value || 0) * 100) / 100;
}

// lineTotal is what one sale line charges: quantity times price, less its discount, plus tax.
// The engine stores line totals after this script runs on a new sale, so the total is worked out
// here until the engine derives child fields before before_save (#824, DERIVE-CHILDREN).
function lineTotal(line) {
  return money(Number(line.quantity || 0) * Number(line.unit_price || 0) -
    Number(line.discount_amount || 0) + Number(line.tax_amount || 0));
}

function saleTotal(sale) {
  var total = 0;
  (sale.items || []).forEach(function (line) {
    total += lineTotal(line);
  });
  return money(total);
}

function baseTotal(sale) {
  return money(saleTotal(sale) * Number(sale.exchange_rate || 1));
}

// owed is what a sale still owes its customer. Only an open credit sale owes.
function owed(sale) {
  if (Number(sale.on_credit || 0) !== 1) return 0;
  if (sale.status !== "Awaiting Payment" && sale.status !== "Partially Paid") return 0;
  return money(baseTotal(sale) - money(sale.base_amount_paid));
}

function adjustCustomer(name, delta) {
  if (!name) throw new Error("A sale on credit needs a customer.");
  var customer = kora.getDoc("Customer", name);
  if (!customer) throw new Error("Cannot check customer " + name + ".");
  kora.saveDoc("Customer", {
    name: name,
    outstanding_balance: money(money(customer.outstanding_balance) + delta)
  });
}

var ref = doc.reference || doc.name || "This sale";
var customer = String(doc.customer || "");

// A sale becomes a credit sale when a person enters it as Awaiting Payment and it has no
// Sale Payment lines (those are register sales). That happens on create, or when a Draft
// is entered as Awaiting Payment.
function entersCredit(sale) {
  return sale.status === "Awaiting Payment" && (sale.payments || []).length === 0;
}

if (!oldDoc) {
  doc.on_credit = entersCredit(doc) ? 1 : 0;
  if (doc.on_credit === 1) {
    doc.base_amount_paid = 0;
  } else if (doc.status === "Paid" || doc.status === "Return") {
    doc.base_amount_paid = baseTotal(doc);
  }
  if (owed(doc) !== 0) adjustCustomer(customer, owed(doc));
  return { doc: doc };
}

var credit = Number(oldDoc.on_credit || 0) === 1;
var enters = !credit && oldDoc.status === "Draft" && entersCredit(doc);
var paidBefore = money(oldDoc.base_amount_paid);
var statusChanged = doc.status !== oldDoc.status;
doc.on_credit = credit || enters ? 1 : 0;
if (enters) {
  doc.base_amount_paid = 0;
}

if (credit) {
  // Only the payment script changes base_amount_paid, in the same save as the status.
  var paymentSave = money(doc.base_amount_paid) !== paidBefore;
  if (statusChanged && (doc.status === "Partially Paid" || doc.status === "Paid") && !paymentSave) {
    throw new Error("A sale on credit is paid by recording payments.");
  }
  if (paidBefore > 0) {
    if (saleTotal(doc) !== saleTotal(oldDoc) ||
        String(doc.currency || "") !== String(oldDoc.currency || "") ||
        customer !== String(oldDoc.customer || "")) {
      throw new Error(ref + " has payments; its total, currency and customer cannot change.");
    }
    if (statusChanged && (doc.status === "Void" || doc.status === "Cancelled")) {
      throw new Error(ref + " has payments. Void its payments first.");
    }
  }
} else if (doc.status === "Paid" || doc.status === "Return") {
  // A register sale paid now, or a held sale paid at the till, is paid in full.
  doc.base_amount_paid = baseTotal(doc);
}

var oldCustomer = String(oldDoc.customer || "");
var before = owed(oldDoc);
var after = owed(doc);
if (oldCustomer === customer) {
  if (after !== before) adjustCustomer(customer, after - before);
} else {
  if (before !== 0) adjustCustomer(oldCustomer, -before);
  if (after !== 0) adjustCustomer(customer, after);
}

return { doc: doc };
