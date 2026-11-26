// An order's status is set by a person (Draft, Ordered, Cancelled) or follows its goods
// receipts (Partly Received, Received). A receipt is the save that raises a line's
// received_qty; the Goods Receipt script makes that save. Items cannot change while anything
// is received, and no line may go below what it received.
var doc = __kora_event__.doc;
var oldDoc = __kora_event__.oldDoc;

function qty(value) {
  return Math.round(Number(value || 0) * 1000000) / 1000000;
}

function receivedByProduct(items) {
  var sums = {};
  for (var i = 0; i < items.length; i++) {
    var product = String(items[i].product || "");
    sums[product] = qty((sums[product] || 0) + qty(items[i].received_qty));
  }
  return sums;
}

function totalReceived(items) {
  var total = 0;
  for (var i = 0; i < items.length; i++) {
    total = qty(total + qty(items[i].received_qty));
  }
  return total;
}

function productList(items) {
  var products = [];
  for (var i = 0; i < items.length; i++) {
    products.push(String(items[i].product || ""));
  }
  return JSON.stringify(products.sort());
}

function sameSums(a, b) {
  var keys = {};
  for (var k in a) { keys[k] = true; }
  for (var j in b) { keys[j] = true; }
  for (var key in keys) {
    if (qty(a[key]) !== qty(b[key])) {
      return false;
    }
  }
  return true;
}

function allReceived(items) {
  for (var i = 0; i < items.length; i++) {
    if (qty(items[i].received_qty) < qty(items[i].quantity)) {
      return false;
    }
  }
  return true;
}

var items = doc.items || [];
var ref = doc.reference || doc.name || "This order";

if (!oldDoc) {
  if (totalReceived(items) > 0) {
    throw new Error(ref + " is new; goods receipts record what is received.");
  }
  if (doc.status === "Partly Received" || doc.status === "Received") {
    throw new Error("An order is received through goods receipts.");
  }
  return { doc: doc };
}

var oldItems = oldDoc.items || [];
var oldReceived = totalReceived(oldItems);
var newReceived = totalReceived(items);
if (oldReceived > 0 || newReceived > 0) {
  if (productList(items) !== productList(oldItems)) {
    throw new Error("Items of " + ref + " cannot change while goods are received on it.");
  }
  for (var i = 0; i < items.length; i++) {
    if (qty(items[i].quantity) < qty(items[i].received_qty)) {
      throw new Error("The quantity of " + items[i].product + " on " + ref + " cannot go below the " + qty(items[i].received_qty) + " received.");
    }
  }
}

var receiptSaved = !sameSums(receivedByProduct(oldItems), receivedByProduct(items));

if (receiptSaved) {
  var before = receivedByProduct(oldItems);
  var after = receivedByProduct(items);
  for (var product in before) {
    if (qty(after[product]) < qty(before[product])) {
      throw new Error("Goods received on " + ref + " cannot be taken back.");
    }
  }
  if (oldDoc.status !== "Ordered" && oldDoc.status !== "Partly Received") {
    throw new Error(ref + " is " + oldDoc.status + "; it takes no more deliveries.");
  }
}

if (receiptSaved) {
  doc.status = allReceived(items) ? "Received" : "Partly Received";
  return { doc: doc };
}

if (doc.status !== oldDoc.status) {
  if (doc.status === "Partly Received" || doc.status === "Received") {
    throw new Error("An order is received through goods receipts.");
  }
  if (oldDoc.status === "Received" || oldDoc.status === "Cancelled") {
    throw new Error(ref + " is " + oldDoc.status + " and cannot change status.");
  }
  var allowed = {
    "Draft>Ordered": true,
    "Draft>Cancelled": true,
    "Ordered>Cancelled": true,
    "Ordered>Draft": true
  };
  if (!allowed[oldDoc.status + ">" + doc.status]) {
    throw new Error(ref + " cannot change from " + oldDoc.status + " to " + doc.status + ".");
  }
  if (oldReceived > 0) {
    throw new Error(ref + " has goods received; it cannot be cancelled or set back to Draft.");
  }
}

return { doc: doc };
