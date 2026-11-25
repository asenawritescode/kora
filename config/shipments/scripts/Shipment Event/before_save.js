// A tracking event is part of the shipment's log: it is refused once saved. Saving one
// moves its shipment to the event's status when that is not backwards; a Delivered event
// also fills the shipment's delivered-on date when it is empty. A note (no status) changes
// nothing. The shipment's own rules run on the shipment save made here.
var doc = __kora_event__.doc;
var oldDoc = __kora_event__.oldDoc;

var ORDER = { "Booked": 0, "Collected": 1, "In transit": 2, "Delivered": 3, "Failed": 3 };
var FINAL = ["Delivered", "Failed"];

if (oldDoc) {
  throw new Error("Tracking event " + doc.name + " cannot be changed after it is saved.");
}

var shipmentName = String(doc.shipment || "");
var shipment = shipmentName ? kora.getDoc("Shipment", shipmentName) : null;
if (!shipment) {
  throw new Error("Cannot check shipment " + (shipmentName || "(none)") + ".");
}

var ref = shipment.reference || shipment.name;
if (FINAL.indexOf(shipment.status) >= 0) {
  throw new Error(ref + " is already " + shipment.status + ".");
}

var status = String(doc.status || "");
if (status && ORDER[status] < ORDER[shipment.status]) {
  throw new Error(ref + " is " + shipment.status + "; it cannot go back to " + status + ".");
}

if (status && status !== shipment.status) {
  var update = { name: shipment.name, status: status };
  if (status === "Delivered" && !shipment.delivered_on) {
    update.delivered_on = doc.event_date;
  }
  kora.saveDoc("Shipment", update);
}

return { doc: doc };
