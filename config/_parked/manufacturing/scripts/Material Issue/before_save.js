// A material issue against a work order: the order must be open. A planned order becomes
// In progress. The stock effect is the Engine's quantity_effect; comparing issued materials
// with the recipe is not checked. Issues are append-only, so this runs on create.
var doc = __kora_event__.doc;
var oldDoc = __kora_event__.oldDoc;

if (oldDoc) {
  return { doc: doc };
}

var order = kora.getDoc("Work Order", doc.work_order);
if (!order) {
  throw new Error("Cannot check work order " + doc.work_order + ".");
}
if (order.status !== "Planned" && order.status !== "In progress") {
  throw new Error((order.reference || order.name) + " is " + order.status + "; only a planned or in-progress order takes materials.");
}
if (order.status === "Planned") {
  kora.saveDoc("Work Order", { name: order.name, status: "In progress" });
}

return { doc: doc };
