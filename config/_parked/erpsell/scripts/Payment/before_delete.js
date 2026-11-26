// A received payment on a credit sale is part of what the sale was paid; void it instead.
var doc = __kora_event__.doc;

if (doc.status === "Received" && doc.sale) {
  var sale = kora.getDoc("Sale", doc.sale);
  if (!sale) throw new Error("Cannot check sale " + doc.sale + ".");
  if (Number(sale.on_credit || 0) === 1) {
    throw new Error((doc.reference || doc.name) + " is received; void it instead of deleting it.");
  }
}

return { doc: doc };
