// Pays supplier bills. Marking a Draft payment Paid applies its allocations to the
// bills (each bill's amount paid rises; its status follows in the bill script).
// Money beyond the listed bills stays as supplier credit. Voiding a Paid payment
// reverses exactly what it applied.
var doc = __kora_event__.doc;
var oldDoc = __kora_event__.oldDoc;

function money(value) {
  return Math.round(Number(value || 0) * 100) / 100;
}

// booksReady is true only when the books (the lib_books library shipped by
// erp-accounting) are loaded; purchasing posts to the books then, and saves
// fine without them (nothing posts).
function booksReady() {
  return typeof booksInstalled === "function" && booksInstalled();
}

// paymentRole maps a payment method to the account the money left.
function paymentRole(method) {
  var roles = { "Cash": "Cash", "Bank Transfer": "Bank", "Card": "Bank", "Other": "Cash Clearing" };
  var role = roles[String(method || "")];
  if (!role) {
    throw new Error("No account for payment method " + method + ".");
  }
  return role;
}

function rowsOf(payment) {
  var rows = payment.allocations || [];
  var out = [];
  for (var i = 0; i < rows.length; i++) {
    out.push({ supplier_bill: String(rows[i].supplier_bill || ""), amount: money(rows[i].amount) });
  }
  return out;
}

function loadBill(name) {
  var bill = kora.getDoc("Supplier Bill", name);
  if (!bill) {
    throw new Error("The bill " + name + " could not be found.");
  }
  return bill;
}

// Each bill's amount paid after the rows, summed per bill.
function sumByBill(rows) {
  var sums = {};
  for (var i = 0; i < rows.length; i++) {
    sums[rows[i].supplier_bill] = money((sums[rows[i].supplier_bill] || 0) + rows[i].amount);
  }
  return sums;
}

function planAllocations(payment) {
  var rows = rowsOf(payment);
  if (rows.length === 0 && payment.supplier_bill) {
    var shortcut = loadBill(payment.supplier_bill);
    var owed = money(money(shortcut.amount) - money(shortcut.amount_paid));
    var pays = Math.min(money(payment.amount), owed);
    rows.push({ supplier_bill: String(payment.supplier_bill), amount: money(pays) });
  }
  var total = 0;
  var pending = {};
  for (var i = 0; i < rows.length; i++) {
    var row = rows[i];
    var bill = loadBill(row.supplier_bill);
    if (String(bill.supplier || "") !== String(payment.supplier || "")) {
      throw new Error(bill.name + " belongs to another supplier.");
    }
    if (String(bill.currency || "") !== String(payment.currency || "")) {
      throw new Error(bill.name + " is in another currency than the payment.");
    }
    if (bill.status !== "Open" && bill.status !== "Partly Paid") {
      throw new Error(bill.name + " is not open for payment (status " + bill.status + ").");
    }
    if (!(row.amount > 0)) {
      throw new Error("An allocation to " + bill.name + " must be more than zero.");
    }
    pending[bill.name] = money((pending[bill.name] || 0) + row.amount);
    var owedNow = money(money(bill.amount) - money(bill.amount_paid));
    if (pending[bill.name] > owedNow) {
      throw new Error(bill.name + " has only " + owedNow + " still owed.");
    }
    total = money(total + row.amount);
  }
  if (total > money(payment.amount)) {
    throw new Error("The bills total " + total + ", more than the payment of " + money(payment.amount) + ".");
  }
  return { rows: rows, total: total, sums: sumByBill(rows) };
}

function sameAllocations(a, b) {
  var left = rowsOf(a);
  var right = rowsOf(b);
  if (left.length !== right.length) {
    return false;
  }
  for (var i = 0; i < left.length; i++) {
    if (left[i].supplier_bill !== right[i].supplier_bill || left[i].amount !== right[i].amount) {
      return false;
    }
  }
  return true;
}

function applyPaid() {
  var plan = planAllocations(doc);
  for (var name in plan.sums) {
    var bill = loadBill(name);
    kora.saveDoc("Supplier Bill", { name: name, amount_paid: money(money(bill.amount_paid) + plan.sums[name]) });
  }
  doc.allocations = plan.rows;
  doc.unallocated_amount = money(money(doc.amount) - plan.total);
  if (booksReady()) {
    var payBase = money(money(doc.amount) * Number(doc.exchange_rate || 1));
    if (payBase !== 0) {
      postEntry("Supplier Payment", doc.name, String(doc.payment_date || "").slice(0, 10), "Supplier payment", [
        { role: "Payable", debit: payBase, credit: 0 },
        { role: paymentRole(doc.method), debit: 0, credit: payBase }
      ]);
    }
  }
}

function reverseAllocations() {
  var sums = sumByBill(rowsOf(oldDoc));
  for (var name in sums) {
    var bill = loadBill(name);
    var left = money(money(bill.amount_paid) - sums[name]);
    kora.saveDoc("Supplier Bill", { name: name, amount_paid: left < 0 ? 0 : left });
  }
  doc.unallocated_amount = 0;
  if (booksReady()) {
    reverseEntry("Supplier Payment", doc.name, "Supplier payment");
  }
}

if (!oldDoc) {
  // Recorded as Draft, or straight to Paid with its allocations applied in the same save.
  if (doc.status === "Paid") {
    applyPaid();
  } else if (doc.status !== "Draft") {
    throw new Error("A payment is recorded as Draft or Paid, not " + doc.status + ".");
  }
  return { doc: doc };
}

if (oldDoc.status === "Paid" && doc.status === "Paid") {
  if (String(doc.supplier || "") !== String(oldDoc.supplier || "") ||
      String(doc.currency || "") !== String(oldDoc.currency || "") ||
      money(doc.amount) !== money(oldDoc.amount) ||
      !sameAllocations(doc, oldDoc)) {
    throw new Error("A paid payment's supplier, currency, amount and bills cannot change. Void it instead.");
  }
} else if (oldDoc.status === "Draft" && doc.status === "Paid") {
  applyPaid();
} else if (oldDoc.status === "Paid" && doc.status === "Voided") {
  reverseAllocations();
} else if (oldDoc.status !== doc.status) {
  throw new Error("A payment cannot change from " + oldDoc.status + " to " + doc.status + ".");
}

return { doc: doc };
