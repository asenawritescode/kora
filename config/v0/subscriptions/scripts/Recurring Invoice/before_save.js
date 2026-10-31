// Keep invoice arithmetic consistent. Issued invoices retain their price;
// payment allocation may still update amount_paid and balance.
var doc = __kora_event__.doc;
var oldDoc = __kora_event__.oldDoc;
var subtotal = Number(doc.subtotal || 0);
var tax = Number(doc.tax_amount || 0);
var amountPaid = Number(doc.amount_paid || 0);

if (subtotal < 0 || tax < 0 || amountPaid < 0) {
  throw new Error("Invoice amounts cannot be negative.");
}
if (oldDoc && oldDoc.status !== "Draft" &&
    (Number(oldDoc.subtotal || 0) !== subtotal || Number(oldDoc.tax_amount || 0) !== tax ||
     String(oldDoc.currency || "") !== String(doc.currency || ""))) {
  throw new Error("An issued invoice cannot be repriced or changed to another currency.");
}

doc.total = Math.round((subtotal + tax) * 100) / 100;
if (amountPaid > doc.total) {
  throw new Error("Payments cannot exceed the invoice total; record any credit separately.");
}
doc.balance = Math.round((doc.total - amountPaid) * 100) / 100;

if (doc.status !== "Draft" && doc.status !== "Void") {
  if (doc.balance === 0) {
    doc.status = "Paid";
  } else if (amountPaid > 0) {
    doc.status = "Partially Paid";
  } else if (doc.status !== "Overdue") {
    doc.status = "Issued";
  }
}

return { doc: doc };
