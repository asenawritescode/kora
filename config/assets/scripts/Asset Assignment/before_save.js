// Who holds an asset, and when. An assignment that is current today puts the asset in use
// with its user; one that has ended clears the asset's "Assigned to" and returns it to store.
// The assignments of one asset may not overlap; an open-ended one runs on indefinitely.
var doc = __kora_event__.doc;
var oldDoc = __kora_event__.oldDoc;
var OPEN_END = "9999-12-31";

function day(value) {
  return value ? String(value).slice(0, 10) : "";
}

var today = new Date().toISOString().slice(0, 10);
var from = day(doc.from_date);
var to = day(doc.to_date) || OPEN_END;

var asset = kora.getDoc("Asset", String(doc.asset || ""));
if (!asset) {
  throw new Error("Cannot check asset " + doc.asset + ".");
}
// Only a new assignment, or one moved to another asset, is refused: closing an old
// assignment of an asset retired since then must still work.
var newHolder = !oldDoc || String(oldDoc.asset || "") !== String(doc.asset || "");
if (newHolder && asset.status === "Retired") {
  throw new Error((asset.asset_name || asset.name) + " is retired and cannot be assigned.");
}

var others = kora.getList("Asset Assignment", { filters: { asset: doc.asset }, limit: 500 });
for (var i = 0; i < others.length; i++) {
  var other = others[i];
  if (doc.name && other.name === doc.name) {
    continue;
  }
  var otherFrom = day(other.from_date);
  var otherTo = day(other.to_date) || OPEN_END;
  if (from <= otherTo && otherFrom <= to) {
    throw new Error((asset.asset_name || asset.name) + " is assigned to " + other.assigned_to + " from " + otherFrom + ".");
  }
}

var changes = null;
if (from <= today && to >= today) {
  changes = { assigned_to: doc.assigned_to };
  if (asset.status === "In store") {
    changes.status = "In use";
  }
} else if (day(doc.to_date) !== "" && day(doc.to_date) < today &&
           String(asset.assigned_to || "") === String(doc.assigned_to || "")) {
  changes = { assigned_to: "" };
  if (asset.status === "In use") {
    changes.status = "In store";
  }
}

if (changes) {
  changes.name = asset.name;
  kora.saveDoc("Asset", changes);
}

return { doc: doc };
