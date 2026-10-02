// Financial validation rules for Kora ERP Kenya
var doc = __kora_event__.doc;
var action = __kora_event__.action;

// Journal Entry: Debits must equal Credits
if (__kora_event__.doctype === "Journal Entry") {
  var items = doc.items || [];
  var totalDebit = 0;
  var totalCredit = 0;
  for (var i = 0; i < items.length; i++) {
    totalDebit += parseFloat(items[i].debit_amount || 0);
    totalCredit += parseFloat(items[i].credit_amount || 0);
  }
  if (Math.abs(totalDebit - totalCredit) > 0.01) {
    throw new Error("Journal entry must balance. Debits (KES " + totalDebit.toFixed(2) + ") must equal Credits (KES " + totalCredit.toFixed(2) + "). Difference: KES " + Math.abs(totalDebit - totalCredit).toFixed(2));
  }
  if (totalDebit === 0 && totalCredit === 0) {
    throw new Error("Journal entry must have at least one debit or credit amount.");
  }
}

// Sales Invoice: Total must be positive
if (__kora_event__.doctype === "Sales Invoice") {
  if (parseFloat(doc.total || 0) <= 0) {
    throw new Error("Invoice total must be greater than zero.");
  }
}

// Prevent editing posted/submitted documents
if (doc.status === "Posted" || doc.status === "Paid" || doc.status === "Approved" || doc.status === "Completed") {
  if (action === "before_save" || action === "validate") {
    throw new Error("Cannot edit a " + doc.status.toLowerCase() + " document. Reverse or cancel it first.");
  }
}
