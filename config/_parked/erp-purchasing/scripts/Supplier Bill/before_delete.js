// A bill with payments is part of the money record; void its payments first.
var doc = __kora_event__.doc;
var paid = Math.round(Number(doc.amount_paid || 0) * 100) / 100;

if (paid > 0) {
  throw new Error((doc.reference || doc.name) + " has payments of " + paid + "; void them first.");
}

return { doc: doc };
