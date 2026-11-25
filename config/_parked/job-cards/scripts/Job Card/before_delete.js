// Deleting a job card gives its parts back to stock. A collected card is part of the
// workshop's record and cannot be deleted.
var doc = __kora_event__.doc;
var ref = doc.reference || doc.name || "This job card";

if (doc.status === "Collected") {
  throw new Error(ref + " is collected and cannot be deleted.");
}

var returns = {};
var parts = doc.parts || [];
for (var i = 0; i < parts.length; i++) {
  var product = String(parts[i].product || "");
  returns[product] = (returns[product] || 0) + Number(parts[i].quantity || 0);
}

// Read every product before writing any stock, so a refusal changes nothing.
var updates = [];
for (var product in returns) {
  if (returns[product] === 0) {
    continue;
  }
  var stock = kora.getDoc("Product", product);
  if (!stock) {
    throw new Error("Cannot check product " + product + ".");
  }
  var have = Number(stock.stock_qty || 0);
  updates.push({ name: product, stock_qty: Math.round((have + returns[product]) * 1000000) / 1000000 });
}
for (var u = 0; u < updates.length; u++) {
  kora.saveDoc("Product", updates[u]);
}

return { doc: doc };
