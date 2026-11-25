// A recipe's materials: no material may be the recipe's own product, and each material
// needs a quantity above zero (the Engine does not check constraints on child table rows).
var doc = __kora_event__.doc;

function qty(value) {
  return Math.round(Number(value || 0) * 1000000) / 1000000;
}

var ref = doc.reference || doc.name || "This recipe";
var rows = doc.components || [];
for (var i = 0; i < rows.length; i++) {
  if (rows[i].material && String(rows[i].material) === String(doc.product || "")) {
    throw new Error(ref + " cannot use its own product as a material.");
  }
  if (!(qty(rows[i].quantity) > 0)) {
    throw new Error(ref + " has a material whose quantity is not above zero.");
  }
}

return { doc: doc };
