// Payments on credit sales. A Received payment settles part of the sale it is linked
// to, and voiding it reverses that. A payment on any other sale changes nothing:
// register sales are paid at the till with Sale Payment lines.
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


// ---- payment and expense roles (#815) ----
// paymentMethodRole maps a Payment or Expense method to the account it moves money through.
function paymentMethodRole(method) {
  var roles = { "Cash": "Cash", "Card": "Bank", "Bank Transfer": "Bank", "Other": "Cash Clearing" };
  var role = roles[String(method || "")];
  if (!role) {
    throw new Error("No account for payment method " + method + ".");
  }
  return role;
}

function paymentToday() {
  return new Date().toISOString().slice(0, 10);
}
// ---- end of payment and expense roles ----

var ref = doc.reference || doc.name || "This payment";
var wasStatus = oldDoc ? oldDoc.status : "";

if (wasStatus === "Voided" && doc.status !== "Voided") {
  throw new Error(ref + " is voided and cannot change.");
}
if (wasStatus === "Received" && (doc.status === "Pending" || doc.status === "Failed")) {
  throw new Error(ref + " is received; void it instead.");
}

var receives = doc.status === "Received" && wasStatus !== "Received";
var reverses = wasStatus === "Received" && doc.status === "Voided";
if (!doc.sale || (!receives && !reverses)) return { doc: doc };

var sale = kora.getDoc("Sale", doc.sale);
if (!sale) throw new Error("Cannot check sale " + doc.sale + ".");
if (Number(sale.on_credit || 0) !== 1) return { doc: doc };

var source = receives ? doc : oldDoc;
var amount = money(Number(source.amount || 0) * Number(source.exchange_rate || 1));
var paid = money(sale.base_amount_paid);
var total = money(sale.base_total);
var outstanding = money(total - paid);
var nextPaid;

if (receives) {
  if (sale.status !== "Awaiting Payment" && sale.status !== "Partially Paid") {
    throw new Error(sale.name + " is already paid; nothing more can be received against it.");
  }
  if (amount > money(outstanding + 0.01)) {
    throw new Error(ref + " is " + amount + ", more than the " + outstanding + " still owed on " + sale.name + ".");
  }
  nextPaid = money(paid + amount);
} else {
  nextPaid = money(Math.max(0, paid - amount));
}

var left = money(total - nextPaid);
var status = nextPaid <= 0 ? "Awaiting Payment" : (left < 0.01 ? "Paid" : "Partially Paid");
kora.saveDoc("Sale", { name: sale.name, base_amount_paid: nextPaid, status: status });

// A payment that settles a credit sale posts Dr its method's account, Cr Receivable. Voiding
// it reverses that entry (nothing happens when it never posted).
if (receives && booksReady()) {
  postEntry("Payment", doc.name, paymentToday(), "Payment", [
    { role: paymentMethodRole(doc.method), debit: amount, credit: 0 },
    { role: "Receivable", debit: 0, credit: amount }
  ]);
}
if (reverses && booksReady()) {
  reverseEntry("Payment", doc.name, "Payment");
}

return { doc: doc };
