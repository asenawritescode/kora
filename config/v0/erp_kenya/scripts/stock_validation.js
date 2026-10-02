// Stock and inventory validation rules
var doc = __kora_event__.doc;

// Stock Movement: Validate quantity
if (__kora_event__.doctype === "Stock Movement") {
  var qty = parseFloat(doc.quantity || 0);

  if (doc.movement_type === "Shipment" || doc.movement_type === "Transfer") {
    // Check available stock
    var item = kora.getDoc("Item", doc.item);
    if (item && qty > parseFloat(item.current_stock || 0)) {
      throw new Error("Insufficient stock for " + doc.item + ". Available: " + item.current_stock + ", Requested: " + qty);
    }
  }

  if (qty <= 0) {
    throw new Error("Movement quantity must be greater than zero.");
  }

  // Transfer must have both from and to warehouses
  if (doc.movement_type === "Transfer" && (!doc.from_warehouse || !doc.to_warehouse)) {
    throw new Error("Transfer type requires both source and destination warehouses.");
  }
  if (doc.movement_type === "Transfer" && doc.from_warehouse === doc.to_warehouse) {
    throw new Error("Source and destination warehouses must be different for a transfer.");
  }
}

// Stock Count: Validate variances
if (__kora_event__.doctype === "Stock Count" && __kora_event__.action === "validate") {
  var items = doc.items || [];
  for (var i = 0; i < items.length; i++) {
    if (parseFloat(items[i].counted_qty || 0) < 0) {
      throw new Error("Counted quantity cannot be negative for item: " + (items[i].item || "unknown"));
    }
  }
}

// Goods Receipt: Validate received qty against PO
if (__kora_event__.doctype === "Goods Receipt Item") {
  var receivedQty = parseFloat(doc.received_qty || 0);
  var orderedQty = parseFloat(doc.ordered_qty || 0);
  if (receivedQty > orderedQty) {
    throw new Error("Received quantity (" + receivedQty + ") cannot exceed ordered quantity (" + orderedQty + ").");
  }
}
