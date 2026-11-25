// Records a repayment against a disbursed loan: the loan's amount repaid rises, and the loan
// becomes Repaid in the same save once nothing is still owed. Runs on create only: a
// repayment is append-only.
var doc = __kora_event__.doc;
var oldDoc = __kora_event__.oldDoc;

function money(value) {
  return Math.round(Number(value || 0) * 100) / 100;
}

if (oldDoc) {
  throw new Error("A loan repayment cannot be changed after saving.");
}

var loan = kora.getDoc("Loan", String(doc.loan || ""));
if (!loan) {
  throw new Error("Cannot check loan " + doc.loan + ".");
}
if (loan.status !== "Disbursed") {
  throw new Error(doc.loan + " is " + loan.status + "; repayments start once it is Disbursed.");
}

var owed = money(money(loan.amount) - money(loan.amount_repaid));
var amount = money(doc.amount);
if (amount > owed) {
  throw new Error("The payment of " + amount + " is more than the " + owed + " still owed on " + doc.loan + ".");
}

var repaid = money(money(loan.amount_repaid) + amount);
var update = { name: loan.name, amount_repaid: repaid };
if (money(money(loan.amount) - repaid) === 0) {
  update.status = "Repaid";
}
kora.saveDoc("Loan", update);
return { doc: doc };
