// A batch's quantity on hand is what was received, less what prescriptions dispensed.
// Changing the quantity received moves quantity on hand by the same difference, and a
// receipt that would leave less than zero is refused. Prescriptions set quantity on hand
// through kora.saveDoc (their own save's rules), so an unchanged receipt leaves it alone.
var doc = __kora_event__.doc;
var oldDoc = __kora_event__.oldDoc;

function qty(value) {
  return Math.round(Number(value || 0) * 1000000) / 1000000;
}

var received = qty(doc.quantity_received);

if (!oldDoc) {
  doc.quantity_on_hand = received;
  return { doc: doc };
}

var onHand = qty(doc.quantity_on_hand) + (received - qty(oldDoc.quantity_received));
if (onHand < 0) {
  throw new Error("Batch " + (doc.batch_no || doc.name) + " would have " + onHand + " on hand; more than prescriptions have taken from it.");
}
doc.quantity_on_hand = onHand;

return { doc: doc };
