// Pays farmer deliveries. Marking a Draft payment Paid applies its allocations to the
// deliveries (each delivery's amount paid rises and its status follows). Money beyond
// the listed deliveries stays as farmer credit. Voiding a Paid payment reverses exactly
// what it applied.
var doc = __kora_event__.doc;
var oldDoc = __kora_event__.oldDoc;

function money(value) {
  return Math.round(Number(value || 0) * 100) / 100;
}

function rowsOf(payment) {
  var rows = payment.allocations || [];
  var out = [];
  for (var i = 0; i < rows.length; i++) {
    out.push({ produce_delivery: String(rows[i].produce_delivery || ""), amount: money(rows[i].amount) });
  }
  return out;
}

function loadDelivery(name) {
  var delivery = kora.getDoc("Produce Delivery", name);
  if (!delivery) {
    throw new Error("The delivery " + name + " could not be found.");
  }
  return delivery;
}

// A delivery's status follows what has been paid on it.
function deliveryStatus(paid, amount) {
  if (paid <= 0) {
    return "Received";
  }
  return paid >= amount ? "Paid" : "Partly Paid";
}

// Each delivery's amount paid by the rows, summed per delivery.
function sumByDelivery(rows) {
  var sums = {};
  for (var i = 0; i < rows.length; i++) {
    sums[rows[i].produce_delivery] = money((sums[rows[i].produce_delivery] || 0) + rows[i].amount);
  }
  return sums;
}

function planAllocations(payment) {
  var rows = rowsOf(payment);
  var total = 0;
  var pending = {};
  for (var i = 0; i < rows.length; i++) {
    var row = rows[i];
    var delivery = loadDelivery(row.produce_delivery);
    if (String(delivery.farmer || "") !== String(payment.farmer || "")) {
      throw new Error(row.produce_delivery + " belongs to another farmer.");
    }
    if (delivery.status !== "Received" && delivery.status !== "Partly Paid") {
      throw new Error(row.produce_delivery + " is not open for payment (status " + delivery.status + ").");
    }
    if (!(row.amount > 0)) {
      throw new Error("An allocation to " + row.produce_delivery + " must be more than zero.");
    }
    pending[row.produce_delivery] = money((pending[row.produce_delivery] || 0) + row.amount);
    var owed = money(money(delivery.amount) - money(delivery.amount_paid));
    if (pending[row.produce_delivery] > owed) {
      throw new Error(row.produce_delivery + " has only " + owed + " still owed.");
    }
    total = money(total + row.amount);
  }
  if (total > money(payment.amount)) {
    throw new Error("The deliveries total " + total + ", more than the payment of " + money(payment.amount) + ".");
  }
  return { rows: rows, total: total, sums: sumByDelivery(rows) };
}

function sameAllocations(a, b) {
  var left = rowsOf(a);
  var right = rowsOf(b);
  if (left.length !== right.length) {
    return false;
  }
  for (var i = 0; i < left.length; i++) {
    if (left[i].produce_delivery !== right[i].produce_delivery || left[i].amount !== right[i].amount) {
      return false;
    }
  }
  return true;
}

function applyPaid() {
  var plan = planAllocations(doc);
  for (var name in plan.sums) {
    var delivery = loadDelivery(name);
    var paid = money(money(delivery.amount_paid) + plan.sums[name]);
    kora.saveDoc("Produce Delivery", {
      name: name,
      amount_paid: paid,
      status: deliveryStatus(paid, money(delivery.amount))
    });
  }
  doc.allocations = plan.rows;
  doc.unallocated_amount = money(money(doc.amount) - plan.total);
}

function reverseAllocations() {
  var sums = sumByDelivery(rowsOf(oldDoc));
  for (var name in sums) {
    var delivery = loadDelivery(name);
    var left = money(money(delivery.amount_paid) - sums[name]);
    if (left < 0) {
      left = 0;
    }
    kora.saveDoc("Produce Delivery", {
      name: name,
      amount_paid: left,
      status: deliveryStatus(left, money(delivery.amount))
    });
  }
  doc.unallocated_amount = 0;
}

if (!oldDoc) {
  // Recorded as Draft, or straight to Paid with its allocations applied in the same save.
  if (doc.status === "Paid") {
    applyPaid();
  } else if (doc.status !== "Draft") {
    throw new Error("A payment is recorded as Draft or Paid, not " + doc.status + ".");
  }
  return { doc: doc };
}

if (oldDoc.status === "Paid" && doc.status === "Paid") {
  if (String(doc.farmer || "") !== String(oldDoc.farmer || "") ||
      money(doc.amount) !== money(oldDoc.amount) ||
      !sameAllocations(doc, oldDoc)) {
    throw new Error("A paid payment's farmer, amount and deliveries cannot change. Void it instead.");
  }
} else if (oldDoc.status === "Draft" && doc.status === "Paid") {
  applyPaid();
} else if (oldDoc.status === "Paid" && doc.status === "Voided") {
  reverseAllocations();
} else if (oldDoc.status !== doc.status) {
  throw new Error("A payment cannot change from " + oldDoc.status + " to " + doc.status + ".");
}

return { doc: doc };
