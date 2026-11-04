// Prepare a draft invoice from the selected subscription. This is deliberately
// limited to a new invoice; later edits must not silently reprice an issued one.
var doc = __kora_event__.doc;
if (!doc.subscription) {
  throw new Error("Choose a subscription before creating an invoice.");
}

var subscription = kora.getDoc("Subscription", doc.subscription);
if (!subscription) {
  throw new Error("The selected subscription could not be found.");
}
if (subscription.status !== "Active") {
  throw new Error("Only an active subscription can be invoiced.");
}

doc.customer = subscription.customer;
doc.currency = subscription.currency;
doc.status = doc.status || "Draft";

if (doc.subtotal === undefined || doc.subtotal === null || doc.subtotal === "") {
  var subtotal = Number(subscription.unit_price || 0) * Number(subscription.quantity || 1);
  var lines = kora.getList("Subscription Line", {
    filters: { subscription: subscription.name, status: "Active" },
    limit: 500
  });
  for (var i = 0; i < lines.length; i++) {
    subtotal += Number(lines[i].unit_price || 0) * Number(lines[i].quantity || 1);
  }

  var plan = kora.getDoc("Plan", subscription.plan);
  var taxRate = plan ? Number(plan.tax_rate || 0) : 0;
  doc.subtotal = Math.round(subtotal * 100) / 100;
  doc.tax_amount = Math.round(subtotal * taxRate) / 100;
}

if (!doc.invoice_number) {
  doc.invoice_number = "INV-" + Date.now();
}
doc.amount_paid = Number(doc.amount_paid || 0);
doc.total = Math.round((Number(doc.subtotal || 0) + Number(doc.tax_amount || 0)) * 100) / 100;
doc.balance = Math.max(0, Math.round((doc.total - doc.amount_paid) * 100) / 100);

return { doc: doc };
