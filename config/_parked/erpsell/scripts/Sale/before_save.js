// Credit sales and register sales. A sale entered as Awaiting Payment is a credit
// sale: its Payment records settle it. A register sale is written as Paid (or Held,
// then Paid) with its Sale Payment lines, and owes nothing. The sale owns the
// customer's outstanding_balance: each save moves it by what the sale owed before
// and what it owes now, so a payment reaches the customer through its sale.
var doc = __kora_event__.doc;
var oldDoc = __kora_event__.oldDoc;

// booksReady is true only when the books (the lib_books library shipped by
// erp-accounting) are loaded; the selling pack posts to the books then, and
// saves fine without them (nothing posts).
function booksReady() {
  return typeof booksInstalled === "function" && booksInstalled();
}

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


// ---- sale books (#815): what a sale, a credit sale and a return post ----
var SALE_PAYMENT_ROLES = { "Cash": "Cash", "Mobile Money": "Mobile Money", "Card": "Bank" };

function salePaymentRole(method) {
  var role = SALE_PAYMENT_ROLES[String(method || "")];
  if (!role) {
    throw new Error("No account for payment method " + method + ".");
  }
  return role;
}

// saleRate turns one payment line into the base currency. A line with no currency is in the
// sale's own currency, so it uses the sale's frozen exchange rate.
function saleRate(sale, code) {
  if (!code) {
    return Number(sale.exchange_rate || 1);
  }
  var found = kora.getList("Currency", { filters: { code: code }, limit: 1 });
  if (found.length === 0) {
    throw new Error("Cannot check currency " + code + ".");
  }
  return Number(found[0].rate_to_base);
}

// saleLine is one signed amount on a role: a positive amount debits it, a negative one credits it.
function saleLine(role, amount) {
  return amount >= 0 ? { role: role, debit: amount, credit: 0 } : { role: role, debit: 0, credit: -amount };
}

// saleCost is what the sold goods cost, in the base currency (product cost prices are base).
function saleCost(sale) {
  var cost = 0;
  (sale.items || []).forEach(function (line) {
    var product = kora.getDoc("Product", String(line.product || ""));
    if (!product) {
      throw new Error("Cannot check product " + line.product + ".");
    }
    cost = postingMoney(cost + Number(line.quantity || 0) * Number(product.cost_price || 0));
  });
  return cost;
}

// saleTaxBase is the tax on the sale, in the base currency.
function saleTaxBase(sale) {
  var tax = 0;
  (sale.items || []).forEach(function (line) {
    tax = postingMoney(tax + Number(line.tax_amount || 0));
  });
  return postingMoney(tax * Number(sale.exchange_rate || 1));
}

// salePayments is the money side of a sale (sign 1) or of its return (sign -1): one signed line
// per payment role. The rows must add up to the base total, to within a cent a row; any rounding
// difference goes on the largest row. A return with no refund rows is refunded in cash.
function salePayments(sale, sign) {
  var total = baseTotal(sale);
  var rows = sale.payments || [];
  if (rows.length === 0 && sign < 0) {
    return [saleLine("Cash", sign * total)];
  }
  var amounts = [];
  var sum = 0;
  var largest = 0;
  for (var i = 0; i < rows.length; i++) {
    var amount = postingMoney(Number(rows[i].amount || 0) * saleRate(sale, String(rows[i].currency || "")));
    amounts.push(amount);
    sum = postingMoney(sum + amount);
    if (amount > amounts[largest]) {
      largest = i;
    }
  }
  var diff = postingMoney(total - sum);
  if (Math.abs(diff) > 0.01 * rows.length + 0.0001) {
    throw new Error("Payments on " + (sale.reference || sale.name) + " add up to " + sum + ", not the sale total " + total + ".");
  }
  var lines = [];
  for (var j = 0; j < rows.length; j++) {
    var value = j === largest ? postingMoney(amounts[j] + diff) : amounts[j];
    lines.push(saleLine(salePaymentRole(rows[j].method), sign * value));
  }
  return lines;
}

// saleEntry is the whole of one sale's entry: the money side (already signed), then revenue,
// tax and cost. Sign -1 gives the opposite entry a return posts.
function saleEntry(sale, sign, moneySide) {
  var tax = saleTaxBase(sale);
  var cost = saleCost(sale);
  return moneySide.concat([
    saleLine("Sales", -sign * postingMoney(baseTotal(sale) - tax)),
    saleLine("Tax Payable", -sign * tax),
    saleLine("Cost of Sales", sign * cost),
    saleLine("Inventory", -sign * cost)
  ]);
}

// postSaleEvents posts the sale's entry when it moves into a posting state, and reverses it
// when the sale is voided or cancelled. old is null on create. Nothing posts when the books
// are not installed.
function postSaleEvents(sale, old) {
  if (!booksReady()) {
    return;
  }
  var day = String(sale.sale_date || "").slice(0, 10);
  var oldStatus = old ? old.status : "";
  var credit = Number(sale.on_credit || 0) === 1;
  var wasCredit = !!old && Number(old.on_credit || 0) === 1;
  if (credit && !wasCredit) {
    postEntry("Sale", sale.name, day, "Sale on credit", saleEntry(sale, 1, [saleLine("Receivable", baseTotal(sale))]));
  }
  if (credit && wasCredit && oldStatus !== sale.status && (sale.status === "Void" || sale.status === "Cancelled")) {
    reverseEntry("Sale", sale.name, "Sale on credit");
  }
  if (!credit && sale.status === "Paid" && oldStatus !== "Paid") {
    postEntry("Sale", sale.name, day, "Sale", saleEntry(sale, 1, salePayments(sale, 1)));
  }
  if (!credit && oldStatus === "Paid" && sale.status === "Void") {
    reverseEntry("Sale", sale.name, "Sale");
  }
  if (sale.status === "Return" && oldStatus !== "Return") {
    postEntry("Sale", sale.name, day, "Return", saleEntry(sale, -1, salePayments(sale, -1)));
  }
}
// ---- end of sale books ----

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
  postSaleEvents(doc, null);
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
postSaleEvents(doc, oldDoc);

return { doc: doc };
