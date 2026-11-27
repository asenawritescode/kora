// A stock adjustment changes its product's stock once, when it is created; it is
// append-only. Its value posts to the books in the same save: an increase debits
// Inventory and credits Stock Adjustments, a decrease does the opposite. The Engine
// signs the quantity by reason; the same signs are applied here, so the value is
// right whether or not the quantity was already signed.
var doc = __kora_event__.doc;
var oldDoc = __kora_event__.oldDoc;

// booksReady is true only when the books (the lib_books library shipped by
// erp-accounting) are loaded; stock adjustments post to the books then, and
// save fine without them (nothing posts).
function booksReady() {
  return typeof booksInstalled === "function" && booksInstalled();
}

var STOCK_SIGNS = { "Opening balance": 1, "Found": 1, "Damaged": -1, "Lost or stolen": -1 };


if (!oldDoc && booksReady()) {
  var product = kora.getDoc("Product", doc.product);
  if (!product) {
    throw new Error("Cannot check product " + doc.product + ".");
  }
  var change = Number(doc.delta || 0);
  var sign = STOCK_SIGNS[String(doc.reason || "").trim()];
  if (sign) {
    change = sign * Math.abs(change);
  }
  var value = postingMoney(change * Number(product.cost_price || 0));
  postEntry("Stock Adjustment", doc.name, new Date().toISOString().slice(0, 10), "Stock adjustment", [
    { role: "Inventory", debit: value, credit: 0 },
    { role: "Stock Adjustments", debit: 0, credit: value }
  ]);
}

return { doc: doc };
