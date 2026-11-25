// A bill's status follows its payments. Payments change amount_paid (through the
// Supplier Payment script); this script derives Partly Paid or Paid from it, and
// refuses status changes a person may not make on a bill that has payments.
var doc = __kora_event__.doc;
var oldDoc = __kora_event__.oldDoc;

function money(value) {
  return Math.round(Number(value || 0) * 100) / 100;
}

var paid = money(doc.amount_paid);
var oldPaid = oldDoc ? money(oldDoc.amount_paid) : 0;
var ref = doc.reference || doc.name || "This bill";

if (paid > 0) {
  if (oldDoc) {
    if (String(doc.supplier || "") !== String(oldDoc.supplier || "") ||
        String(doc.currency || "") !== String(oldDoc.currency || "") ||
        money(doc.amount) !== money(oldDoc.amount)) {
      throw new Error(ref + " has payments of " + paid + "; its supplier, currency and amount cannot change.");
    }
    if (doc.status !== oldDoc.status && ["Draft", "Open", "Cancelled"].indexOf(doc.status) >= 0) {
      throw new Error(ref + " has payments of " + paid + "; void them first.");
    }
  }
  if (paid >= money(doc.amount)) {
    doc.status = "Paid";
  } else {
    doc.status = "Partly Paid";
  }
} else if (doc.status === "Partly Paid" || doc.status === "Paid") {
  if (oldPaid > 0) {
    // The last payment was voided: the bill is open again.
    doc.status = "Open";
  } else {
    throw new Error("A bill becomes Partly Paid or Paid by recording a Supplier Payment.");
  }
}

return { doc: doc };
