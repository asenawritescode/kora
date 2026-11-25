// A shipment's status moves forward only: Booked, Collected, In transit, then Delivered or
// Failed. Delivered and Failed are final. A person's change and a tracking event's change
// both pass through here. A new shipment starts Booked and is not checked.
var doc = __kora_event__.doc;
var oldDoc = __kora_event__.oldDoc;

var ORDER = { "Booked": 0, "Collected": 1, "In transit": 2, "Delivered": 3, "Failed": 3 };
var FINAL = ["Delivered", "Failed"];

var ref = doc.reference || doc.name || "This shipment";

if (oldDoc && doc.status !== oldDoc.status) {
  if (FINAL.indexOf(oldDoc.status) >= 0) {
    throw new Error(ref + " is " + oldDoc.status + "; it cannot change to " + doc.status + ".");
  }
  if (ORDER[doc.status] < ORDER[oldDoc.status]) {
    throw new Error(ref + " is " + oldDoc.status + "; it cannot go back to " + doc.status + ".");
  }
}

return { doc: doc };
