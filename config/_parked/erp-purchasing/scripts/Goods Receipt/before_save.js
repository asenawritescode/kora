// A receipt against a purchase order takes its lines against that order: the order must be
// Ordered or Partly Received and from the same supplier, each product must be on it, and no
// product may exceed what is still due. The order's lines then take the quantities in line
// order, and the order's status follows (the Purchase Order script).
var doc = __kora_event__.doc;

if (!doc.purchase_order) {
  return { doc: doc };
}

function qty(value) {
  return Math.round(Number(value || 0) * 1000000) / 1000000;
}

var orderName = String(doc.purchase_order);
var order = kora.getDoc("Purchase Order", orderName);
if (!order) {
  throw new Error("Cannot check purchase order " + orderName + ".");
}
var ref = order.reference || order.name;

if (String(order.supplier || "") !== String(doc.supplier || "")) {
  throw new Error(ref + " is for another supplier.");
}
if (order.status !== "Ordered" && order.status !== "Partly Received") {
  if (order.status === "Draft") {
    throw new Error(ref + " is Draft; mark it Ordered first.");
  }
  throw new Error(ref + " is " + order.status + "; it takes no more deliveries.");
}

var lines = order.items || [];
var due = {};
for (var i = 0; i < lines.length; i++) {
  var product = String(lines[i].product || "");
  due[product] = qty((due[product] || 0) + qty(lines[i].quantity) - qty(lines[i].received_qty));
}

var receiving = {};
var rows = doc.items || [];
for (var j = 0; j < rows.length; j++) {
  var name = String(rows[j].product || "");
  var amount = qty(rows[j].quantity);
  if (!(amount > 0)) {
    throw new Error("The quantity of " + name + " must be more than zero.");
  }
  if (!Object.prototype.hasOwnProperty.call(due, name)) {
    throw new Error(name + " is not on " + ref + ".");
  }
  receiving[name] = qty((receiving[name] || 0) + amount);
}
for (var product2 in receiving) {
  if (receiving[product2] > due[product2]) {
    throw new Error("Only " + qty(due[product2]) + " of " + product2 + " are still due on " + ref + ".");
  }
}

var updated = [];
for (var k = 0; k < lines.length; k++) {
  var line = lines[k];
  var take = qty(Math.min(qty(line.quantity) - qty(line.received_qty), receiving[String(line.product || "")] || 0));
  if (take > 0) {
    receiving[String(line.product || "")] = qty(receiving[String(line.product || "")] - take);
  }
  updated.push({
    name: line.name,
    product: line.product,
    quantity: line.quantity,
    unit_cost: line.unit_cost,
    received_qty: qty(qty(line.received_qty) + take)
  });
}
kora.saveDoc("Purchase Order", { name: order.name, items: updated });

return { doc: doc };
