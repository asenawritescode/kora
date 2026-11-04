// Apply later provider-confirmed payments and reverse prior allocations exactly
// once when a successful payment is explicitly marked Reversed.
var doc = __kora_event__.doc;
var oldDoc = __kora_event__.oldDoc;
if (!oldDoc) {
  return { doc: doc };
}
if (oldDoc.status === "Succeeded" && doc.status === "Failed") {
  throw new Error("A succeeded payment must be marked Reversed, not Failed.");
}
if (oldDoc.status === doc.status) {
  return { doc: doc };
}

var invoice = kora.getDoc("Recurring Invoice", oldDoc.invoice);
if (!invoice) {
  throw new Error("The payment's invoice could not be found.");
}
if (String(doc.invoice || "") !== String(oldDoc.invoice || "") ||
    Number(doc.amount || 0) !== Number(oldDoc.amount || 0) ||
    String(doc.currency || "") !== String(oldDoc.currency || "")) {
  throw new Error("A payment's invoice, amount, and currency cannot be changed after creation.");
}

if (oldDoc.status !== "Succeeded" && doc.status === "Succeeded") {
  if (Number(doc.amount) > Number(invoice.balance || 0)) {
    throw new Error("Payment amount cannot exceed the invoice balance.");
  }
  var paid = Math.round((Number(invoice.amount_paid || 0) + Number(doc.amount)) * 100) / 100;
  kora.saveDoc("Recurring Invoice", {
    name: invoice.name,
    amount_paid: paid,
    status: paid >= Number(invoice.total || 0) ? "Paid" : "Partially Paid"
  });
} else if (oldDoc.status === "Succeeded" && doc.status === "Reversed") {
  var reversedPaid = Math.max(0, Math.round((Number(invoice.amount_paid || 0) - Number(oldDoc.amount || 0)) * 100) / 100);
  kora.saveDoc("Recurring Invoice", {
    name: invoice.name,
    amount_paid: reversedPaid,
    status: reversedPaid === 0 ? "Issued" : "Partially Paid"
  });
}

return { doc: doc };
