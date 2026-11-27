// Cash put into or taken out of a drawer posts when it is recorded. Cash in and a float
// top-up move cash from the clearing account into the drawer; cash out does the reverse; a
// safe drop moves no money between the books' accounts and posts nothing.
var doc = __kora_event__.doc;
var oldDoc = __kora_event__.oldDoc;

// booksReady is true only when the books (the lib_books library shipped by
// erp-accounting) are loaded; the selling pack posts to the books then, and
// saves fine without them (nothing posts).
function booksReady() {
  return typeof booksInstalled === "function" && booksInstalled();
}


// ---- payment and expense roles (#815) ----
// paymentMethodRole maps a Payment or Expense method to the account it moves money through.
function paymentMethodRole(method) {
  var roles = { "Cash": "Cash", "Card": "Bank", "Bank Transfer": "Bank", "Other": "Cash Clearing" };
  var role = roles[String(method || "")];
  if (!role) {
    throw new Error("No account for payment method " + method + ".");
  }
  return role;
}

function paymentToday() {
  return new Date().toISOString().slice(0, 10);
}
// ---- end of payment and expense roles ----

// cashMovementLines gives the entry for one movement type, or null when it posts nothing.
function cashMovementLines(type, amount) {
  if (type === "Cash in" || type === "Float top-up") {
    return [{ role: "Cash", debit: amount, credit: 0 }, { role: "Cash Clearing", debit: 0, credit: amount }];
  }
  if (type === "Cash out") {
    return [{ role: "Cash Clearing", debit: amount, credit: 0 }, { role: "Cash", debit: 0, credit: amount }];
  }
  if (type === "Safe drop") {
    return null;
  }
  throw new Error("Unknown cash movement type " + type + ".");
}

if (!oldDoc && booksReady()) {
  var amount = postingMoney(Number(doc.amount || 0) * Number(doc.exchange_rate || 1));
  var lines = cashMovementLines(String(doc.movement_type || ""), amount);
  if (lines) {
    postEntry("Cash Movement", doc.name, paymentToday(), "Cash movement", lines);
  }
}

return { doc: doc };
