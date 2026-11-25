// Dispensing takes each line's quantity from its batch, all or nothing. Cancelling a
// dispensed prescription returns the quantities. A dispensed prescription's medicines are
// frozen, and a cancelled prescription's status is final. Product stock is not changed.
var doc = __kora_event__.doc;
var oldDoc = __kora_event__.oldDoc;

function qty(value) {
  return Math.round(Number(value || 0) * 1000000) / 1000000;
}

function dayOf(value) {
  return String(value || "").substring(0, 10);
}

function lineRows(prescription) {
  var rows = prescription.lines || [];
  var out = [];
  for (var i = 0; i < rows.length; i++) {
    out.push({ product: String(rows[i].product || ""), batch: String(rows[i].batch || ""), quantity: qty(rows[i].quantity) });
  }
  return out;
}

function sameLines(a, b) {
  var left = lineRows(a);
  var right = lineRows(b);
  if (left.length !== right.length) {
    return false;
  }
  for (var i = 0; i < left.length; i++) {
    if (left[i].product !== right[i].product || left[i].batch !== right[i].batch || left[i].quantity !== right[i].quantity) {
      return false;
    }
  }
  return true;
}

function loadBatch(name) {
  var batch = kora.getDoc("Medicine Batch", name);
  if (!batch) {
    throw new Error("Cannot check batch " + name + ".");
  }
  return batch;
}

// Each batch's total quantity across the rows, and the batch records loaded once.
function totalsByBatch(rows, ref, forDispense) {
  var totals = {};
  var batches = {};
  for (var i = 0; i < rows.length; i++) {
    var row = rows[i];
    if (!row.batch) {
      throw new Error("Medicine " + row.product + " on " + ref + " needs a batch to dispense.");
    }
    if (!batches[row.batch]) {
      batches[row.batch] = loadBatch(row.batch);
    }
    var batch = batches[row.batch];
    if (String(batch.product || "") !== row.product) {
      throw new Error("Batch " + (batch.batch_no || row.batch) + " is not for " + row.product + ".");
    }
    if (forDispense) {
      var expiry = dayOf(batch.expiry_date);
      if (!expiry) {
        throw new Error("Cannot check batch " + (batch.batch_no || row.batch) + ".");
      }
      if (expiry < new Date().toISOString().substring(0, 10)) {
        throw new Error("Batch " + (batch.batch_no || row.batch) + " expired on " + expiry + ".");
      }
    }
    totals[row.batch] = qty((totals[row.batch] || 0) + row.quantity);
  }
  return { totals: totals, batches: batches };
}

function dispense() {
  var rows = lineRows(doc);
  var ref = doc.reference || doc.name || "This prescription";
  var plan = totalsByBatch(rows, ref, true);
  for (var name in plan.totals) {
    var onHand = qty(plan.batches[name].quantity_on_hand);
    if (plan.totals[name] > onHand) {
      throw new Error("Batch " + (plan.batches[name].batch_no || name) + " has only " + onHand + " on hand, not " + plan.totals[name] + ".");
    }
  }
  for (var batchName in plan.totals) {
    var left = qty(qty(plan.batches[batchName].quantity_on_hand) - plan.totals[batchName]);
    kora.saveDoc("Medicine Batch", { name: batchName, quantity_on_hand: left });
  }
}

function returnLines() {
  var ref = oldDoc.reference || oldDoc.name || "This prescription";
  var plan = totalsByBatch(lineRows(oldDoc), ref, false);
  for (var name in plan.totals) {
    var back = qty(qty(plan.batches[name].quantity_on_hand) + plan.totals[name]);
    kora.saveDoc("Medicine Batch", { name: name, quantity_on_hand: back });
  }
}

var ref = doc.reference || doc.name || "This prescription";
var rows = lineRows(doc);
for (var i = 0; i < rows.length; i++) {
  if (!(rows[i].quantity > 0)) {
    throw new Error("Every medicine on " + ref + " needs a quantity above zero.");
  }
}

if (!oldDoc) {
  if (doc.status === "Dispensed") {
    dispense();
  }
  return { doc: doc };
}

var from = oldDoc.status;
var to = doc.status;

if (from === "Cancelled") {
  if (to !== "Cancelled") {
    throw new Error(ref + " is cancelled; its status cannot change.");
  }
} else if (from === "Received") {
  if (to === "Dispensed") {
    dispense();
  }
} else if (from === "Dispensed") {
  if (!sameLines(doc, oldDoc)) {
    throw new Error(ref + " is dispensed; its medicines cannot change.");
  }
  if (to === "Cancelled") {
    returnLines();
  } else if (to !== "Dispensed") {
    throw new Error(ref + " is dispensed and cannot go back to " + to + ".");
  }
}

return { doc: doc };
