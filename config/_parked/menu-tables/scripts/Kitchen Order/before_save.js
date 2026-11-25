// A kitchen order takes its table: a new order makes a Free or Reserved table Occupied
// (the table's own record is saved here, so a Waiter who may only read tables can take an order).
// Quantities and menu items are checked on a new order and whenever the lines change; on an update
// only new or changed lines must be available, so an item marked unavailable later does not block
// editing an old order. The Engine's required check already refuses an order with no lines.
var doc = __kora_event__.doc;
var oldDoc = __kora_event__.oldDoc;

function qty(value) {
  return Number(value || 0);
}

function flag(value) {
  return value === true || value === 1 || value === "1" || value === "true";
}

function sameLine(line, other) {
  return String(line.menu_item || "") === String(other.menu_item || "") &&
    qty(line.quantity) === qty(other.quantity) &&
    String(line.note || "") === String(other.note || "");
}

// The line of the saved order with the same row name, or null for a new row.
function savedLine(line) {
  var rows = oldDoc ? (oldDoc.lines || []) : [];
  for (var i = 0; i < rows.length; i++) {
    if (line.name && rows[i].name === line.name) {
      return rows[i];
    }
  }
  return null;
}

function linesChanged(lines) {
  if (!oldDoc) {
    return true;
  }
  var saved = oldDoc.lines || [];
  if (saved.length !== lines.length) {
    return true;
  }
  for (var i = 0; i < lines.length; i++) {
    var before = savedLine(lines[i]);
    if (!before || !sameLine(lines[i], before)) {
      return true;
    }
  }
  return false;
}

function takeTable() {
  if (!doc.dining_table) {
    throw new Error("Choose the table for this order.");
  }
  var table = kora.getDoc("Dining Table", String(doc.dining_table));
  if (!table) {
    throw new Error("Cannot check table " + doc.dining_table + ".");
  }
  if (table.status === "Free" || table.status === "Reserved") {
    kora.saveDoc("Dining Table", { name: table.name, status: "Occupied" });
  }
}

function checkLine(line, isNew) {
  var item = kora.getDoc("Menu Item", String(line.menu_item || ""));
  if (!item) {
    throw new Error("Cannot check menu item " + line.menu_item + ".");
  }
  var label = item.item_name || line.menu_item;
  if (qty(line.quantity) < 1) {
    throw new Error("Quantity must be at least 1 for " + label + ".");
  }
  if (isNew && !flag(item.available)) {
    throw new Error(label + " is not available.");
  }
}

function checkLines() {
  var lines = doc.lines || [];
  if (!linesChanged(lines)) {
    return;
  }
  for (var i = 0; i < lines.length; i++) {
    var before = savedLine(lines[i]);
    checkLine(lines[i], !before || !sameLine(lines[i], before));
  }
}

if (!oldDoc) {
  takeTable();
}
checkLines();

return { doc: doc };
