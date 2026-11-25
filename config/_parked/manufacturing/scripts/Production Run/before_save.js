// A production run against a work order: the order must be open and the run's product
// must be the recipe's product. The run adds its quantity to the order's quantity made,
// and a planned order becomes In progress. Runs are append-only, so this runs on create.
var doc = __kora_event__.doc;
var oldDoc = __kora_event__.oldDoc;

function qty(value) {
  return Math.round(Number(value || 0) * 1000000) / 1000000;
}

if (oldDoc || !doc.work_order) {
  return { doc: doc };
}

var order = kora.getDoc("Work Order", doc.work_order);
if (!order) {
  throw new Error("Cannot check work order " + doc.work_order + ".");
}
var orderRef = order.reference || order.name;
if (order.status !== "Planned" && order.status !== "In progress") {
  throw new Error(orderRef + " is " + order.status + "; only a planned or in-progress order takes production.");
}

var recipe = kora.getDoc("Bill of Materials", order.bom);
if (!recipe) {
  throw new Error("Cannot check bill of materials " + order.bom + ".");
}
if (String(recipe.product || "") !== String(doc.product || "")) {
  var made = kora.getDoc("Product", recipe.product);
  throw new Error(orderRef + " makes " + ((made && made.reference) || recipe.product) + ".");
}

var next = { name: order.name, quantity_produced: qty(qty(order.quantity_produced) + qty(doc.quantity)) };
if (order.status === "Planned") {
  next.status = "In progress";
}
kora.saveDoc("Work Order", next);

return { doc: doc };
