// A work order's progress and status. Production runs add to quantity_produced and a
// material issue moves a planned order to In progress (their scripts do that). This
// script checks that the recipe is Active when the order is created or its recipe
// changes, that Done needs the full quantity made, and that Done and Cancelled are final.
var doc = __kora_event__.doc;
var oldDoc = __kora_event__.oldDoc;

function qty(value) {
  return Math.round(Number(value || 0) * 1000000) / 1000000;
}

var ref = doc.reference || doc.name || "This work order";

if (!oldDoc || String(doc.bom || "") !== String(oldDoc.bom || "")) {
  var recipe = kora.getDoc("Bill of Materials", doc.bom);
  if (!recipe) {
    throw new Error("Cannot check bill of materials " + doc.bom + ".");
  }
  if (recipe.status !== "Active") {
    throw new Error((recipe.reference || recipe.name) + " is " + recipe.status + "; " + (recipe.status === "Draft" ? "activate it first." : "choose an active bill of materials."));
  }
}

if (oldDoc && (oldDoc.status === "Done" || oldDoc.status === "Cancelled") && doc.status !== oldDoc.status) {
  throw new Error(ref + " is " + oldDoc.status + "; it cannot change.");
}

if (doc.status === "Done" && qty(doc.quantity_produced) < qty(doc.quantity)) {
  throw new Error("Produced " + qty(doc.quantity_produced) + " of " + qty(doc.quantity) + ".");
}

return { doc: doc };
