// Payments on credit sales. A Received payment settles part of the sale it is linked
// to, and voiding it reverses that. A payment on any other sale changes nothing:
// register sales are paid at the till with Sale Payment lines.
var doc = __kora_event__.doc;
var oldDoc = __kora_event__.oldDoc;

function money(value) {
  return Math.round(Number(value || 0) * 100) / 100;
}

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

return { doc: doc };
