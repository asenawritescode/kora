// Job card rules: parts, the stock they take, status moves, and a collected card is frozen.
// A new card's parts are taken off stock by the card's quantity effect. Later changes to the
// parts move stock here, by the difference per product.
var doc = __kora_event__.doc;
var oldDoc = __kora_event__.oldDoc;

var ref = doc.reference || doc.name || "This job card";
var parts = doc.parts || [];

function qty(value) {
  return Number(value || 0);
}

function money(value) {
  return Math.round(Number(value || 0) * 100) / 100;
}

// productTotals sums the quantity of each product over a list of part rows.
function productTotals(rows) {
  var sums = {};
  for (var i = 0; i < rows.length; i++) {
    var product = String(rows[i].product || "");
    sums[product] = (sums[product] || 0) + qty(rows[i].quantity);
  }
  return sums;
}

// partsSignature is the same text for the same parts, whatever order the rows came in.
function partsSignature(rows) {
  var lines = [];
  for (var i = 0; i < rows.length; i++) {
    lines.push(String(rows[i].product || "") + "=" + qty(rows[i].quantity));
  }
  return lines.sort().join("\n");
}

for (var i = 0; i < parts.length; i++) {
  if (qty(parts[i].quantity) <= 0) {
    throw new Error("Part " + (parts[i].product || "") + " needs a quantity above zero.");
  }
}

var moves = {
  "Booked": ["In progress"],
  "In progress": ["Waiting for parts", "Done"],
  "Waiting for parts": ["In progress"],
  "Done": ["Collected", "In progress"],
  "Collected": []
};

if (!oldDoc) {
  if (doc.status !== "Booked" && doc.status !== "In progress") {
    throw new Error("A new job card starts as Booked or In progress.");
  }
} else if (doc.status !== oldDoc.status) {
  var allowed = moves[oldDoc.status] || [];
  if (oldDoc.status === "Collected") {
    throw new Error(ref + " is collected and cannot change status.");
  }
  if (allowed.indexOf(doc.status) < 0) {
    throw new Error(ref + " cannot move from " + oldDoc.status + " to " + doc.status + ". Allowed: " + allowed.join(", ") + ".");
  }
}

if (oldDoc && oldDoc.status === "Collected") {
  if (partsSignature(parts) !== partsSignature(oldDoc.parts || [])) {
    throw new Error(ref + " is collected; its parts cannot change.");
  }
  if (money(doc.labour_charge) !== money(oldDoc.labour_charge)) {
    throw new Error(ref + " is collected; its labour charge cannot change.");
  }
}

if (oldDoc) {
  var before = productTotals(oldDoc.parts || []);
  var after = productTotals(parts);
  var changed = [];
  var product;
  for (product in after) {
    changed.push({ product: product, change: after[product] - (before[product] || 0) });
  }
  for (product in before) {
    if (!(product in after)) {
      changed.push({ product: product, change: -before[product] });
    }
  }
  // Check every product before writing any stock, so a refusal changes nothing.
  var updates = [];
  for (var c = 0; c < changed.length; c++) {
    if (changed[c].change === 0) {
      continue;
    }
    var stock = kora.getDoc("Product", changed[c].product);
    if (!stock) {
      throw new Error("Cannot check product " + changed[c].product + ".");
    }
    var have = qty(stock.stock_qty);
    var left = Math.round((have - changed[c].change) * 1000000) / 1000000;
    if (left < 0) {
      throw new Error("Only " + have + " of " + changed[c].product + " in stock.");
    }
    updates.push({ name: changed[c].product, stock_qty: left });
  }
  for (var u = 0; u < updates.length; u++) {
    kora.saveDoc("Product", updates[u]);
  }
}

return { doc: doc };
