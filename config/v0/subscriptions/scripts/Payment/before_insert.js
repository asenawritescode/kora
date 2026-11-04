// A successful payment is applied to its invoice in the same transaction as
// the payment record. Pending/failed/reversed records remain unapplied.
var doc = __kora_event__.doc;
if (!doc.invoice) {
  throw new Error("Choose an invoice before recording a payment.");
}
if (!(Number(doc.amount) > 0)) {
  throw new Error("Payment amount must be greater than zero.");
}

var invoice = kora.getDoc("Recurring Invoice", doc.invoice);
if (!invoice) {
  throw new Error("The selected invoice could not be found.");
}
if (invoice.status === "Void") {
  throw new Error("A void invoice cannot receive a payment.");
}
doc.subscription = invoice.subscription;
doc.customer = invoice.customer;
doc.currency = invoice.currency;
doc.status = doc.status || "Succeeded";
if (!doc.payment_reference) {
  doc.payment_reference = "PAY-" + Date.now();
}
if (doc.currency !== invoice.currency) {
  throw new Error("Payment currency must match the invoice currency.");
}

if (doc.status === "Succeeded") {
  var balance = Number(invoice.balance || 0);
  if (Number(doc.amount) > balance) {
    throw new Error("Payment amount cannot exceed the invoice balance.");
  }
  var amountPaid = Math.round((Number(invoice.amount_paid || 0) + Number(doc.amount)) * 100) / 100;
  kora.saveDoc("Recurring Invoice", {
    name: invoice.name,
    amount_paid: amountPaid,
    status: amountPaid >= Number(invoice.total || 0) ? "Paid" : "Partially Paid"
  });
}

return { doc: doc };
