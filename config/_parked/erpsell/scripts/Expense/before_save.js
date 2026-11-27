// Money the shop spends posts to the books when it is recorded: Dr Expenses, Cr the account
// it was paid from. An expense is not re-posted when it is edited.
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

if (!oldDoc && booksReady()) {
  var amount = postingMoney(Number(doc.amount || 0) * Number(doc.exchange_rate || 1));
  postEntry("Expense", doc.name, String(doc.expense_date || "").slice(0, 10), "Expense", [
    { role: "Expenses", debit: amount, credit: 0 },
    { role: paymentMethodRole(doc.payment_method), debit: 0, credit: amount }
  ]);
}

return { doc: doc };
