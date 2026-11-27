// A bill's status follows its payments. Payments change amount_paid (through the
// Supplier Payment script); this script derives Partly Paid or Paid from it, and
// refuses status changes a person may not make on a bill that has payments.
var doc = __kora_event__.doc;
var oldDoc = __kora_event__.oldDoc;

function money(value) {
  return Math.round(Number(value || 0) * 100) / 100;
}

// booksReady is true only when the books (the lib_books library shipped by
// erp-accounting) are loaded; purchasing posts to the books then, and saves
// fine without them (nothing posts).
function booksReady() {
  return typeof booksInstalled === "function" && booksInstalled();
}

var paid = money(doc.amount_paid);
var oldPaid = oldDoc ? money(oldDoc.amount_paid) : 0;
var ref = doc.reference || doc.name || "This bill";

if (paid > 0) {
  if (oldDoc) {
    if (String(doc.supplier || "") !== String(oldDoc.supplier || "") ||
        String(doc.currency || "") !== String(oldDoc.currency || "") ||
        money(doc.amount) !== money(oldDoc.amount)) {
      throw new Error(ref + " has payments of " + paid + "; its supplier, currency and amount cannot change.");
    }
    if (doc.status !== oldDoc.status && ["Draft", "Open", "Cancelled"].indexOf(doc.status) >= 0) {
      throw new Error(ref + " has payments of " + paid + "; void them first.");
    }
  }
  if (paid >= money(doc.amount)) {
    doc.status = "Paid";
  } else {
    doc.status = "Partly Paid";
  }
} else if (doc.status === "Partly Paid" || doc.status === "Paid") {
  if (oldPaid > 0) {
    // The last payment was voided: the bill is open again.
    doc.status = "Open";
  } else {
    throw new Error("A bill becomes Partly Paid or Paid by recording a Supplier Payment.");
  }
}

// A bill against a goods receipt or a purchase order is checked against what was received.
// Amounts are compared in the base currency across an order; a receipt needs the same currency.
function baseOf(amount, rate) {
  return Math.round(Number(amount || 0) * Number(rate || 1) * 100) / 100;
}

function checkReceipt(doc, ref) {
  var receiptName = String(doc.goods_receipt || "");
  if (receiptName === "") {
    return null;
  }
  var receipt = kora.getDoc("Goods Receipt", receiptName);
  if (!receipt) {
    throw new Error("Cannot check goods receipt " + receiptName + ".");
  }
  if (String(receipt.supplier || "") !== String(doc.supplier || "")) {
    throw new Error(ref + " is from another supplier than " + receiptName + ".");
  }
  if (String(receipt.currency || "") !== String(doc.currency || "")) {
    throw new Error(ref + " is in another currency than " + receiptName + ".");
  }
  if (money(doc.amount) > money(receipt.total)) {
    throw new Error(ref + " is " + money(doc.amount) + ", more than " + receiptName + " of " + money(receipt.total) + ".");
  }
  return receipt;
}

function checkOrder(doc, ref, receipt) {
  var orderName = String(doc.purchase_order || "");
  if (orderName === "") {
    return;
  }
  var order = kora.getDoc("Purchase Order", orderName);
  if (!order) {
    throw new Error("Cannot check purchase order " + orderName + ".");
  }
  if (String(order.supplier || "") !== String(doc.supplier || "")) {
    throw new Error(ref + " is from another supplier than " + orderName + ".");
  }
  if (receipt && String(receipt.purchase_order || "") !== orderName) {
    throw new Error(ref + " names " + orderName + " but " + receipt.name + " belongs to " + (receipt.purchase_order || "no order") + ".");
  }

  var receipts = kora.getList("Goods Receipt", { filters: { purchase_order: orderName }, limit: 500 });
  if (receipts.length >= 500) {
    throw new Error("Cannot check " + orderName + ": too many records.");
  }
  var orderLines = order.items || [];
  var orderReceived = 0;
  for (var k = 0; k < orderLines.length; k++) {
    orderReceived = orderReceived + Number(orderLines[k].received_qty || 0);
  }
  if (receipts.length === 0 && orderReceived > 0) {
    throw new Error("Cannot check " + orderName + ": its goods receipts could not be read.");
  }
  var received = 0;
  for (var i = 0; i < receipts.length; i++) {
    received = received + baseOf(receipts[i].total, receipts[i].exchange_rate);
  }

  var bills = kora.getList("Supplier Bill", { filters: { purchase_order: orderName }, limit: 500 });
  if (bills.length >= 500) {
    throw new Error("Cannot check " + orderName + ": too many records.");
  }
  var billed = 0;
  for (var j = 0; j < bills.length; j++) {
    if (bills[j].name === doc.name || bills[j].status === "Cancelled") {
      continue;
    }
    billed = billed + baseOf(bills[j].amount, bills[j].exchange_rate);
  }
  if (doc.status !== "Cancelled") {
    billed = billed + baseOf(doc.amount, doc.exchange_rate);
  }
  if (Math.round(billed * 100) > Math.round(received * 100)) {
    throw new Error("The bills on " + orderName + " would total " + Math.round(billed * 100) / 100 + ", more than the " + Math.round(received * 100) / 100 + " received.");
  }
}

var receiptRecord = checkReceipt(doc, ref);
checkOrder(doc, ref, receiptRecord);

// The books: a recognized bill owes the supplier. A bill against a goods receipt
// clears the receipt's holding account (Dr Goods Received Not Billed); a bill with
// no receipt is an expense (Dr Expenses). Either way Cr Payable, in the base
// currency. A cancelled (unpaid) bill reverses. Posted once, by posting role.
if (booksReady()) {
  var billBase = baseOf(doc.amount, doc.exchange_rate);
  var recognized = doc.status === "Open" || doc.status === "Partly Paid" || doc.status === "Paid";
  if (recognized && billBase !== 0) {
    var debitRole = String(doc.goods_receipt || "") !== "" ? "Goods Received Not Billed" : "Expenses";
    postEntry("Supplier Bill", doc.name, String(doc.bill_date || "").slice(0, 10), "Supplier bill", [
      { role: debitRole, debit: billBase, credit: 0 },
      { role: "Payable", debit: 0, credit: billBase }
    ]);
  }
  if (doc.status === "Cancelled") {
    reverseEntry("Supplier Bill", doc.name, "Supplier bill");
  }
}

return { doc: doc };
