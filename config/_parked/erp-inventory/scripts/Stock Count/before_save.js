// A stock count is submitted once. On submit the Engine sets each product's stock to the
// counted number (moving it by the line's difference); the same difference posts to the books
// in one entry: the value counted short is a loss (Dr Stock Adjustments / Cr Inventory) and the
// value counted over is a gain (the opposite). Lines net together, so one entry covers the count.
var doc = __kora_event__.doc;
var oldDoc = __kora_event__.oldDoc;

// booksReady is true only when the books (the lib_books library shipped by
// erp-accounting) are loaded; the count posts to the books then, and submits
// fine without them (nothing posts).
function booksReady() {
  return typeof booksInstalled === "function" && booksInstalled();
}

var wasStatus = oldDoc ? oldDoc.status : "";
var submits = doc.status === "Submitted" && wasStatus !== "Submitted";

if (submits && booksReady()) {
  var lines = [];
  (doc.items || []).forEach(function (line) {
    var product = kora.getDoc("Product", String(line.product || ""));
    if (!product) {
      throw new Error("Cannot check product " + line.product + ".");
    }
    var value = postingMoney(Number(line.difference || 0) * Number(product.cost_price || 0));
    if (value !== 0) {
      lines.push({ role: "Inventory", debit: value, credit: 0 });
      lines.push({ role: "Stock Adjustments", debit: 0, credit: value });
    }
  });
  postEntry("Stock Count", doc.name, new Date().toISOString().slice(0, 10), "Stock count", lines);
}

return { doc: doc };
